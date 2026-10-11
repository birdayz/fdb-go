package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func TestBatchValueVariantsShapeAndPersistence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		size int
	}{{"batchset50", 100}, {"batchset50x1000", 1000}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prefix := "clientbench/" + strings.Repeat("x", 55) + "/" // 68 bytes, same as campaign.
			d := Prepare(prefix, 2, 3, 5)
			d.prepareBatchSet50(prefix, batchValueBytes(tc.name))
			if d.BatchValueBytes != tc.size {
				t.Fatalf("value size %d, want %d", d.BatchValueBytes, tc.size)
			}
			r := &batchShapeRecorder{}
			if err := d.Seed(r); err != nil {
				t.Fatal(err)
			}
			if len(r.readOnly) != 1 || r.readOnly[0] || len(r.sets) != 205 {
				t.Fatal("seed is not one write transaction with original105 plus100 batch Sets")
			}
			for worker, keys := range d.BatchWrites {
				for slot, key := range keys {
					v := r.values[string(key)]
					if len(v) != tc.size || v[0] != 'i' || binary.LittleEndian.Uint64(v[1:9]) != uint64(worker) || binary.LittleEndian.Uint64(v[17:25]) != uint64(slot) {
						t.Fatalf("bad seeded worker%d slot%d", worker, slot)
					}
				}
			}
			for phaseIndex, phase := range [][][]byte{d.Warmup, d.Measure} {
				wantPhase := byte('w')
				if phaseIndex == 1 {
					wantPhase = 'm'
				}
				for index, payload := range phase {
					r.readOnly, r.sets, r.gets = nil, nil, nil
					attempts, err := d.Operation(r, tc.name, index%2, payload)
					if err != nil || attempts != 1 || len(r.readOnly) != 1 || r.readOnly[0] || len(r.sets) != 50 || len(r.gets) != 0 {
						t.Fatalf("wrong write transaction shape: attempts=%d err=%v tx=%v sets=%d gets=%d", attempts, err, r.readOnly, len(r.sets), len(r.gets))
					}
					keys, values := map[string]bool{}, map[string]bool{}
					for slot, kv := range r.sets {
						wantKey := fmt.Sprintf("%sbatchset50/%04d/%04d", prefix, index%2, slot)
						if string(kv.Key) != wantKey || len(kv.Key) != 88 || keys[string(kv.Key)] || len(kv.Value) != tc.size || values[string(kv.Value)] {
							t.Fatalf("bad key/value at slot%d", slot)
						}
						keys[string(kv.Key)], values[string(kv.Value)] = true, true
						if kv.Value[0] != wantPhase || binary.LittleEndian.Uint64(kv.Value[1:9]) != uint64(index%2) || binary.LittleEndian.Uint64(kv.Value[9:17]) != uint64(index) || binary.LittleEndian.Uint64(kv.Value[17:25]) != uint64(slot) {
							t.Fatal("phase/worker/operation/slot identity mismatch")
						}
						for i := 25; i < tc.size; i++ {
							if kv.Value[i] != byte((i*31+17)%251) {
								t.Fatalf("payload byte mismatch at %d", i)
							}
						}
					}
				}
			}
			r.readOnly, r.sets, r.gets = nil, nil, nil
			if err := d.ValidateWrites(r, tc.name, 3, 5); err != nil {
				t.Fatal(err)
			}
			if len(r.readOnly) != 1 || !r.readOnly[0] || len(r.gets) != 102 || len(r.sets) != 0 {
				t.Fatal("final validation must read all100 batch keys+2 original worker keys in one read-only transaction")
			}
			for worker, keys := range d.BatchWrites {
				for slot, key := range keys {
					original := bytes.Clone(r.values[string(key)])
					r.values[string(key)][tc.size-1] ^= 1
					err := d.ValidateWrites(r, tc.name, 3, 5)
					if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("worker %d slot %d:", worker, slot)) {
						t.Fatalf("same-length corruption accepted: worker%d slot%d: %v", worker, slot, err)
					}
					r.values[string(key)] = original
				}
			}
			cfg := Config{ClusterFile: "cluster", Output: "output", AllowWrites: true, Prefix: prefix, Iterations: 100, Warmup: 40, Workers: 1, TimeoutMS: 30000, RetryLimit: 10, Workload: tc.name, Profile: "none"}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			other := "batchset50"
			if tc.name == other {
				other = "batchset50x1000"
			}
			r.sets = nil
			if _, err := d.Operation(r, other, 0, d.Measure[0]); err == nil || len(r.sets) != 0 {
				t.Fatal("wrong workload size accepted")
			}
			if err := d.ValidateWrites(r, other, 3, 5); err == nil {
				t.Fatal("wrong-size validation accepted")
			}
		})
	}
}

func TestLargeBatchRoundRobinBoundaries(t *testing.T) {
	t.Parallel()
	for _, counts := range [][2]int{{0, 0}, {2, 0}, {5, 2}, {0, 5}, {3, 7}} {
		t.Run(fmt.Sprint(counts), func(t *testing.T) {
			t.Parallel()
			d := Prepare("clientbench/v6-boundaries/", 4, counts[0], counts[1])
			d.prepareBatchSet50("clientbench/v6-boundaries/", 1000)
			r := &batchShapeRecorder{}
			if err := d.Seed(r); err != nil {
				t.Fatal(err)
			}
			for _, phase := range [][][]byte{d.Warmup, d.Measure} {
				for i, payload := range phase {
					if _, err := d.Operation(r, "batchset50x1000", i%4, payload); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := d.ValidateWrites(r, "batchset50x1000", counts[0], counts[1]); err != nil {
				t.Fatal(err)
			}
		})
	}
}

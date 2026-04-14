package client

import (
	"bytes"
	"context"
	"sort"
	"testing"
)

// FuzzRYWCache verifies the read-your-writes cache by comparing its output
// against a simple map-based model. The fuzzer generates random sequences of
// Set/Clear/ClearRange/Atomic operations, applies them to both the RYW cache
// and a model, then verifies Get and GetRange produce identical results.
//
// This is the most important fuzz target for the pure Go client — RYW merge
// logic is the most complex code path and any bug here means silent data
// corruption (reads returning wrong values).
//
// Covers all 12 atomic mutation types plus Set/Clear/ClearRange. Each atomic
// op is modeled using the same applyAtomic() function used by the RYW cache,
// so divergence means the cache's merge/resolution has a bug.
func FuzzRYWCache(f *testing.F) {
	// Seed corpus: hand-crafted operation sequences.
	// Format: repeated [op_type, key_idx, val_idx] triplets.
	// op_type % 16 selects operation (see switch below).
	f.Add([]byte{0, 0, 1, 1, 1, 1, 2, 2, 2})          // set, clear, clearRange
	f.Add([]byte{0, 0, 0, 0, 0, 0})                   // set same key twice
	f.Add([]byte{2, 0, 3})                            // clearRange
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 2, 0, 3}) // set, clear, set, clearRange
	f.Add([]byte{3, 2, 1, 3, 2, 2})                   // AtomicAdd twice on same key
	f.Add([]byte{0, 2, 1, 4, 2, 3})                   // Set then AtomicOr
	f.Add([]byte{12, 2, 1})                           // CompareAndClear
	f.Add([]byte{3, 0, 0, 12, 0, 0})                  // AtomicAdd then CompareAndClear
	f.Add([]byte{5, 2, 0, 4, 2, 1})                   // AtomicAnd then AtomicOr
	f.Add([]byte{1, 2, 0, 3, 2, 1})                   // Clear then AtomicAdd on absent
	f.Add([]byte{7, 2, 0, 8, 2, 1})                   // AtomicMax then AtomicMin
	f.Add([]byte{9, 0, 1, 10, 0, 2})                  // ByteMax then ByteMin
	f.Add([]byte{11, 4, 0, 11, 4, 1, 11, 4, 2})       // AppendIfFits chain

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 3 {
			return
		}

		// Keys: k00..k09 (10 keys). Values: v0..v3 (byte strings).
		keys := []string{"k00", "k01", "k02", "k03", "k04", "k05", "k06", "k07", "k08", "k09"}
		values := [][]byte{
			{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // le64(1)
			{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // le64(2)
			{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // le64(3)
			{0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // le64(255)
		}

		// Server state: seed with some initial data (8-byte LE values for
		// compatibility with all atomic types).
		serverData := map[string][]byte{
			"k00": {0x0A, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 10
			"k02": {0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 20
			"k04": {0x1E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 30
			"k06": {0x28, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 40
			"k08": {0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 50
		}

		// Model: starts as copy of server.
		model := make(map[string][]byte)
		for k, v := range serverData {
			cp := make([]byte, len(v))
			copy(cp, v)
			model[k] = cp
		}

		// Server callbacks (immutable server state).
		serverGet := func(_ context.Context, key []byte) ([]byte, error) {
			return serverData[string(key)], nil
		}
		serverGetRange := func(_ context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
			var result []KeyValue
			for k, v := range serverData {
				if bytes.Compare([]byte(k), begin) >= 0 && bytes.Compare([]byte(k), end) < 0 {
					result = append(result, KeyValue{Key: []byte(k), Value: v})
				}
			}
			sort.Slice(result, func(i, j int) bool {
				cmp := bytes.Compare(result[i].Key, result[j].Key)
				if reverse {
					return cmp > 0
				}
				return cmp < 0
			})
			if limit > 0 && len(result) > limit {
				result = result[:limit]
				return result, true, nil
			}
			return result, false, nil
		}

		// RYW cache.
		cache := &rywCache{}

		// All supported atomic mutation types for fuzzing.
		atomicTypes := []MutationType{
			MutAddValue, MutOr, MutAnd, MutAndV2, MutXor,
			MutMax, MutMin, MutMinV2,
			MutByteMax, MutByteMin,
			MutAppendIfFits, MutCompareAndClear,
		}

		// Parse and apply operations.
		pos := 0
		for pos+2 < len(data) {
			opType := data[pos] % 16
			keyIdx := int(data[pos+1]) % len(keys)
			valIdx := int(data[pos+2]) % len(values)
			pos += 3

			k := keys[keyIdx]
			param := values[valIdx]

			switch {
			case opType == 0: // Set
				cache.set([]byte(k), param)
				model[k] = append([]byte(nil), param...)

			case opType == 1: // Clear
				cache.clear([]byte(k))
				delete(model, k)

			case opType == 2: // ClearRange [keyIdx, keyIdx+2)
				endIdx := keyIdx + 2
				if endIdx > len(keys) {
					endIdx = len(keys)
				}
				begin := keys[keyIdx]
				end := keys[endIdx-1] + "\x00"
				cache.clearRange([]byte(begin), []byte(end))
				for _, ck := range keys[keyIdx:endIdx] {
					delete(model, ck)
				}

			default: // Atomic mutation (opType 3..14 → atomicTypes[0..11])
				atomicIdx := int(opType-3) % len(atomicTypes)
				mutType := atomicTypes[atomicIdx]

				cache.atomic(mutType, []byte(k), param)

				// Model: apply the same atomic function.
				existing := model[k]
				result, cleared := applyAtomic(mutType, existing, param)
				if cleared {
					delete(model, k)
				} else {
					model[k] = result
				}
			}
		}

		ctx := context.Background()

		// Verify Get for all keys.
		for _, k := range keys {
			got, err := cache.get(ctx, []byte(k), serverGet)
			if err != nil {
				t.Fatalf("get(%q): %v", k, err)
			}
			want := model[k]
			if !bytes.Equal(got, want) {
				t.Errorf("get(%q): got %v, want %v", k, got, want)
			}
		}

		// Verify forward GetRange.
		gotKVs, _, err := cache.getRange(ctx, []byte("k00"), []byte("k99"), 100, false, serverGetRange)
		if err != nil {
			t.Fatalf("getRange: %v", err)
		}
		var wantKVs []KeyValue
		for k, v := range model {
			if k >= "k00" && k < "k99" {
				wantKVs = append(wantKVs, KeyValue{Key: []byte(k), Value: v})
			}
		}
		sort.Slice(wantKVs, func(i, j int) bool {
			return bytes.Compare(wantKVs[i].Key, wantKVs[j].Key) < 0
		})
		if len(gotKVs) != len(wantKVs) {
			t.Errorf("forward getRange: got %d keys, want %d", len(gotKVs), len(wantKVs))
			return
		}
		for i := range gotKVs {
			if !bytes.Equal(gotKVs[i].Key, wantKVs[i].Key) || !bytes.Equal(gotKVs[i].Value, wantKVs[i].Value) {
				t.Errorf("forward getRange[%d]: got {%q,%v}, want {%q,%v}",
					i, gotKVs[i].Key, gotKVs[i].Value, wantKVs[i].Key, wantKVs[i].Value)
			}
		}

		// Verify reverse GetRange.
		gotRev, _, err := cache.getRange(ctx, []byte("k00"), []byte("k99"), 100, true, serverGetRange)
		if err != nil {
			t.Fatalf("reverse getRange: %v", err)
		}
		sort.Slice(wantKVs, func(i, j int) bool {
			return bytes.Compare(wantKVs[i].Key, wantKVs[j].Key) > 0
		})
		if len(gotRev) != len(wantKVs) {
			t.Errorf("reverse getRange: got %d keys, want %d", len(gotRev), len(wantKVs))
			return
		}
		for i := range gotRev {
			if !bytes.Equal(gotRev[i].Key, wantKVs[i].Key) || !bytes.Equal(gotRev[i].Value, wantKVs[i].Value) {
				t.Errorf("reverse getRange[%d]: got {%q,%v}, want {%q,%v}",
					i, gotRev[i].Key, gotRev[i].Value, wantKVs[i].Key, wantKVs[i].Value)
			}
		}
	})
}

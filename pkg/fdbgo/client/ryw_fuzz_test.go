package client

import (
	"bytes"
	"context"
	"sort"
	"testing"
)

// FuzzRYWCache verifies the read-your-writes cache by comparing its output
// against a simple map-based model. The fuzzer generates random sequences of
// Set/Clear/ClearRange operations, applies them to both the RYW cache and a
// model, then verifies Get and GetRange produce identical results.
//
// This is the most important fuzz target for the pure Go client — RYW merge
// logic is the most complex code path and any bug here means silent data
// corruption (reads returning wrong values).
func FuzzRYWCache(f *testing.F) {
	// Seed corpus: a few hand-crafted operation sequences.
	// Format: [op_count] then repeated [op_type, key_idx, val_idx]
	// op_type: 0=set, 1=clear, 2=clearRange(key_idx..key_idx+1)
	f.Add([]byte{3, 0, 0, 1, 0, 1, 1, 0, 2, 2})          // set k0=v1, clear k1, clearRange k2..k3
	f.Add([]byte{2, 0, 0, 0, 0, 0, 0})                   // set k0=v0, set k0=v0 (overwrite)
	f.Add([]byte{1, 2, 0, 3})                            // clearRange k0..k4
	f.Add([]byte{4, 0, 0, 0, 0, 1, 0, 0, 2, 0, 2, 0, 3}) // set, clear, set same key, clearRange

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 1 {
			return
		}

		// Keys: k00..k09 (10 keys). Values: v0..v3.
		keys := []string{"k00", "k01", "k02", "k03", "k04", "k05", "k06", "k07", "k08", "k09"}
		values := []string{"v0", "v1", "v2", "v3"}

		// Server state: seed with some initial data.
		serverData := map[string][]byte{
			"k00": []byte("s0"),
			"k02": []byte("s2"),
			"k04": []byte("s4"),
			"k06": []byte("s6"),
			"k08": []byte("s8"),
		}

		// Model: starts as copy of server.
		model := make(map[string][]byte)
		for k, v := range serverData {
			model[k] = v
		}

		// Server callbacks.
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

		// Parse and apply operations.
		pos := 0
		for pos < len(data) {
			if pos+2 >= len(data) {
				break
			}
			opType := data[pos] % 3
			keyIdx := int(data[pos+1]) % len(keys)
			valIdx := int(data[pos+2]) % len(values)
			pos += 3

			switch opType {
			case 0: // Set
				k := keys[keyIdx]
				v := values[valIdx]
				cache.set([]byte(k), []byte(v))
				model[k] = []byte(v)
			case 1: // Clear
				k := keys[keyIdx]
				cache.clear([]byte(k))
				delete(model, k)
			case 2: // ClearRange [keyIdx, keyIdx+2)
				endIdx := keyIdx + 2
				if endIdx > len(keys) {
					endIdx = len(keys)
				}
				begin := keys[keyIdx]
				end := keys[endIdx-1] + "\x00" // exclusive end (includes keys[endIdx-1])
				cache.clearRange([]byte(begin), []byte(end))
				for _, k := range keys[keyIdx:endIdx] {
					delete(model, k)
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
				t.Errorf("get(%q): got %q, want %q", k, got, want)
			}
		}

		// Verify forward GetRange over entire key space (limit 100 to keep fuzzer fast).
		gotKVs, _, err := cache.getRange(ctx, []byte("k00"), []byte("k99"), 100, false, serverGetRange)
		if err != nil {
			t.Fatalf("getRange: %v", err)
		}

		// Build expected from model.
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
			t.Errorf("getRange: got %d keys, want %d", len(gotKVs), len(wantKVs))
			return
		}
		for i := range gotKVs {
			if !bytes.Equal(gotKVs[i].Key, wantKVs[i].Key) || !bytes.Equal(gotKVs[i].Value, wantKVs[i].Value) {
				t.Errorf("getRange[%d]: got {%q,%q}, want {%q,%q}",
					i, gotKVs[i].Key, gotKVs[i].Value, wantKVs[i].Key, wantKVs[i].Value)
			}
		}
	})
}

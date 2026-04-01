package types

import (
	"encoding/binary"
	"testing"
)

func TestParseKeyValueRefVector(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := ParseKeyValueRefVector(nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		data := make([]byte, 4) // count=0
		if got := ParseKeyValueRefVector(data); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("single", func(t *testing.T) {
		t.Parallel()
		data := packKVVector([]KeyValueRef{{Key: []byte("k"), Value: []byte("v")}})
		got := ParseKeyValueRefVector(data)
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
		if string(got[0].Key) != "k" || string(got[0].Value) != "v" {
			t.Errorf("got key=%q value=%q", got[0].Key, got[0].Value)
		}
	})

	t.Run("multiple", func(t *testing.T) {
		t.Parallel()
		input := []KeyValueRef{
			{Key: []byte("alpha"), Value: []byte("1")},
			{Key: []byte("beta"), Value: []byte("22")},
			{Key: []byte("gamma"), Value: []byte("333")},
		}
		data := packKVVector(input)
		got := ParseKeyValueRefVector(data)
		if len(got) != 3 {
			t.Fatalf("expected 3, got %d", len(got))
		}
		for i, kv := range got {
			if string(kv.Key) != string(input[i].Key) || string(kv.Value) != string(input[i].Value) {
				t.Errorf("[%d] got key=%q value=%q, want key=%q value=%q",
					i, kv.Key, kv.Value, input[i].Key, input[i].Value)
			}
		}
	})

	t.Run("truncated_key", func(t *testing.T) {
		t.Parallel()
		data := packKVVector([]KeyValueRef{{Key: []byte("hello"), Value: []byte("world")}})
		// Truncate inside the key data
		got := ParseKeyValueRefVector(data[:8])
		if len(got) != 0 {
			t.Errorf("expected 0 on truncated data, got %d", len(got))
		}
	})

	t.Run("empty_values", func(t *testing.T) {
		t.Parallel()
		input := []KeyValueRef{
			{Key: []byte("k"), Value: []byte{}},
		}
		data := packKVVector(input)
		got := ParseKeyValueRefVector(data)
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
		if string(got[0].Key) != "k" || len(got[0].Value) != 0 {
			t.Errorf("got key=%q value=%q", got[0].Key, got[0].Value)
		}
	})
}

// packKVVector builds VecSerStrategy::String wire format from KeyValue pairs.
func packKVVector(kvs []KeyValueRef) []byte {
	size := 4 // count
	for _, kv := range kvs {
		size += 4 + len(kv.Key) + 4 + len(kv.Value)
	}
	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf, uint32(len(kvs)))
	pos := 4
	for _, kv := range kvs {
		binary.LittleEndian.PutUint32(buf[pos:], uint32(len(kv.Key)))
		pos += 4
		copy(buf[pos:], kv.Key)
		pos += len(kv.Key)
		binary.LittleEndian.PutUint32(buf[pos:], uint32(len(kv.Value)))
		pos += 4
		copy(buf[pos:], kv.Value)
		pos += len(kv.Value)
	}
	return buf
}

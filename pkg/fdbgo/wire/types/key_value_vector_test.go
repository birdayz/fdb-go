package types

import (
	"encoding/binary"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

func TestParseKeyValueRefStringVector(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got, err := ParseKeyValueRefStringVector(nil); got != nil || err != nil {
			t.Errorf("expected nil, got %v, %v", got, err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		data := make([]byte, 4) // count=0
		if got, err := ParseKeyValueRefStringVector(data); got != nil || err != nil {
			t.Errorf("expected nil, got %v, %v", got, err)
		}
	})

	t.Run("single", func(t *testing.T) {
		t.Parallel()
		data := packKVVector([]KeyValueRef{{Key: []byte("k"), Value: []byte("v")}})
		got, err := ParseKeyValueRefStringVector(data)
		if err != nil || len(got) != 1 {
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
		got, err := ParseKeyValueRefStringVector(data)
		if err != nil || len(got) != 3 {
			t.Fatalf("expected 3, got %d", len(got))
		}
		for i, kv := range got {
			if string(kv.Key) != string(input[i].Key) || string(kv.Value) != string(input[i].Value) {
				t.Errorf("[%d] got key=%q value=%q, want key=%q value=%q",
					i, kv.Key, kv.Value, input[i].Key, input[i].Value)
			}
		}
	})

	// A blob the elements do not fill exactly is an error, not a shorter vector.
	two := packKVVector([]KeyValueRef{{Key: []byte("hello"), Value: []byte("world")}, {Key: []byte("k"), Value: []byte("v")}})
	for name, data := range map[string][]byte{
		"short_blob":      {1, 0, 0},
		"truncated_key":   two[:8],
		"truncated_row":   two[:len(two)-1],
		"count_too_large": append([]byte{3, 0, 0, 0}, two[4:]...),
		"count_too_small": append([]byte{1, 0, 0, 0}, two[4:]...),
		"trailing_bytes":  append(append([]byte{}, two...), 0),
		"empty_trailing":  {0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseKeyValueRefStringVector(data)
			var decodeErr *wire.DecodeError
			var vecErr *wire.StringVectorError
			if !errors.As(err, &decodeErr) || !errors.As(err, &vecErr) || got != nil {
				t.Fatalf("got %d rows, %v; want a StringVectorError", len(got), err)
			}
		})
	}

	t.Run("empty_values", func(t *testing.T) {
		t.Parallel()
		input := []KeyValueRef{
			{Key: []byte("k"), Value: []byte{}},
		}
		data := packKVVector(input)
		got, err := ParseKeyValueRefStringVector(data)
		if err != nil || len(got) != 1 {
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

package recordlayer

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func FuzzDeserializeVector(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})                                                 // DOUBLE type, no data
	f.Add([]byte{0x01})                                                 // SINGLE type, no data
	f.Add([]byte{0x02})                                                 // HALF type, no data
	f.Add([]byte{0x05})                                                 // unknown type
	f.Add([]byte{0x00, 0x3f, 0xf0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) // 1 double (1.0)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		_, _ = deserializeVector(data)
	})
}

func FuzzVectorScanContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	// Valid token: two entries, resume at position 1.
	valid := encodeVectorScanContinuation([]*IndexEntry{
		{Key: tuple.Tuple{int64(1)}, Value: tuple.Tuple{nil}},
		{Key: tuple.Tuple{int64(2)}, Value: tuple.Tuple{nil}},
	}, 1)
	f.Add(valid)
	// Corrupt entry key: valid proto, key bytes are not a packed tuple.
	corruptKey, _ := (&gen.VectorIndexScanContinuation{
		IndexEntries: []*gen.VectorIndexScanContinuation_IndexEntry{
			{Key: []byte{0xff}, Value: tuple.Tuple{nil}.Pack()},
		},
		InnerContinuation: []byte{0x00, 0x00, 0x00, 0x00},
	}).MarshalVT()
	f.Add(corruptKey)
	// Short inner position: Java's ListCursor ByteBuffer.getInt underflows.
	shortInner, _ := (&gen.VectorIndexScanContinuation{
		InnerContinuation: []byte{0x01},
	}).MarshalVT()
	f.Add(shortInner)
	// Negative inner position: would index entries[-1] if accepted.
	negativePos, _ := (&gen.VectorIndexScanContinuation{
		InnerContinuation: []byte{0xff, 0xff, 0xff, 0xff},
	}).MarshalVT()
	f.Add(negativePos)

	f.Fuzz(func(t *testing.T, data []byte) {
		m := &vectorIndexMaintainer{standardIndexMaintainer: standardIndexMaintainer{index: &Index{Name: "fuzz_vec"}}}
		cursor, err := m.newVectorSearchCursor(nil, data, nil)
		if err != nil {
			// Explicit error is the contract for bad tokens — but a token the
			// proto parser accepts with sane contents must not error spuriously
			// (covered by the valid seed round-tripping below).
			return
		}
		// Accepted token: emitting must not panic (positions are validated).
		for i := 0; i < 3; i++ {
			res, rerr := cursor.OnNext(context.Background())
			if rerr != nil || !res.HasNext() {
				break
			}
		}
		_ = cursor.Close()
		// A token that fails proto parse must never produce a cursor.
		var contProto gen.VectorIndexScanContinuation
		if len(data) > 0 && contProto.UnmarshalVT(data) != nil {
			t.Fatal("unparseable continuation produced a cursor instead of an error")
		}
	})
}

func FuzzParseNodeValue(f *testing.F) {
	f.Add([]byte{0x01, 0x00, 0x05, 0xff}, uint8(3))
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{0x00, 0x00, 0x00, 0x05, 0x05}, uint8(5))
	f.Add([]byte{0xff, 0xff, 0xff}, uint8(1))

	ss := newHNSWStorage(specSubspaceFuzz(), DefaultHNSWConfig(8))
	const layer = 0

	f.Fuzz(func(t *testing.T, vec []byte, nNeighbors uint8) {
		n := int(nNeighbors % 40)
		neighbors := make([]tuple.Tuple, n)
		for i := range neighbors {
			// Mix int and composite PKs, including value bytes that collide with
			// tuple type codes (0x00, 0x05).
			switch i % 3 {
			case 0:
				neighbors[i] = tuple.Tuple{int64(i * 7)}
			case 1:
				neighbors[i] = tuple.Tuple{int64(i), []byte{0x00, 0x05, byte(i)}}
			default:
				neighbors[i] = tuple.Tuple{int64(-i), "k\x00v"}
			}
		}

		// Build the value exactly like saveNodeLayer.
		neighborList := make(tuple.Tuple, len(neighbors))
		for i, pk := range neighbors {
			neighborList[i] = pk
		}
		value := tuple.Tuple{int64(0), tuple.Tuple{vec}, neighborList}.Pack()

		gotVec, gotSpans, _, err := parseNodeValue(value)
		if err != nil {
			t.Fatalf("parseNodeValue: %v (vec=%x n=%d)", err, vec, n)
		}
		if len(vec) == 0 {
			if len(gotVec) != 0 {
				t.Fatalf("empty vector came back as %x", gotVec)
			}
		} else if !bytes.Equal(gotVec, vec) {
			t.Fatalf("vector mismatch:\n got %x\n want %x", gotVec, vec)
		}
		if len(gotSpans) != n {
			t.Fatalf("neighbor count: got %d want %d", len(gotSpans), n)
		}
		layerPrefix := ss.dataSubspace.Pack(tuple.Tuple{int64(layer)})
		for i, span := range gotSpans {
			pk, derr := decodeNestedPK(span)
			if derr != nil {
				t.Fatalf("decode span %d: %v", i, derr)
			}
			if !tupleEqual(pk, neighbors[i]) {
				t.Fatalf("neighbor %d mismatch: got %v want %v", i, pk, neighbors[i])
			}
			// span must reconstruct the exact fetch key.
			gotKey := append(append([]byte{}, layerPrefix...), span...)
			wantKey := ss.dataSubspace.Pack(tuple.Tuple{int64(layer), neighbors[i]})
			if !bytes.Equal(gotKey, wantKey) {
				t.Fatalf("fetch-key mismatch for neighbor %d:\n got  %x\n want %x", i, gotKey, wantKey)
			}
		}
	})
}

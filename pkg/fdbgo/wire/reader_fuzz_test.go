package wire

// Fuzz the wire Reader constructor and field access methods.
// NewReader parses FlatBuffers headers — crafted data must not panic.

import "testing"

func FuzzNewReader(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Add(make([]byte, 100))
	f.Add([]byte{0x0F, 0xDB, 0x00, 0xB0, 0x73, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := NewReader(data)
		if err != nil {
			return
		}
		// Exercise ALL field access methods — none should panic.
		_ = r.VTableLength()
		for i := 0; i < 10; i++ {
			_ = r.FieldPresent(i)
			_ = r.ReadInt8(i)
			_ = r.ReadUint8(i)
			_ = r.ReadInt16(i)
			_ = r.ReadUint16(i)
			_ = r.ReadInt32(i)
			_ = r.ReadUint32(i)
			_ = r.ReadInt64(i)
			_ = r.ReadUint64(i)
			_ = r.ReadFloat64(i)
			_ = r.ReadBool(i)
			_ = r.ReadBytes(i)
			_ = r.ReadString(i)
			_ = r.ReadUID(i)
			_ = r.ReadVectorInt32(i)
			_ = r.ReadVectorUint64(i)
			_, _ = r.ReadVectorCount(i)
			_, _ = r.ReadNestedReader(i)
			_, _ = r.ReadVectorElementReader(i, 0)
			_, _ = r.ReadUIDPair(i)
			_, _ = r.ReadOptionalInt32(i, i+1)
			_, _ = r.ReadOptionalString(i, i+1)
		}

		// Chained nested reader access — tests ReadNestedReader → ReadNestedReader
		// and ReadNestedReader → field access on the sub-reader. Exercises
		// potential self-referential / circular offset patterns in crafted data.
		for depth := 0; depth < 3; depth++ {
			nr, nerr := r.ReadNestedReader(depth)
			if nerr != nil {
				continue
			}
			// Read fields on nested reader.
			_ = nr.ReadInt64(0)
			_ = nr.ReadBytes(1)
			_ = nr.ReadString(2)
			// Go one level deeper.
			nr2, nerr2 := nr.ReadNestedReader(0)
			if nerr2 != nil {
				continue
			}
			_ = nr2.ReadInt64(0)
			_ = nr2.ReadBytes(1)
			// Vector element on nested reader.
			if cnt, _ := nr.ReadVectorCount(3); cnt > 0 {
				elem, eerr := nr.ReadVectorElementReader(3, 0)
				if eerr == nil {
					_ = elem.ReadInt64(0)
				}
			}
		}
	})
}

func FuzzReadErrorOrInto(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Add(make([]byte, 50))

	f.Fuzz(func(t *testing.T, data []byte) {
		var r Reader
		_ = ReadErrorOrInto(data, &r)
	})
}

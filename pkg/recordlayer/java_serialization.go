package recordlayer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// A reader of the Java Object Serialization stream protocol (the Java Object
// Serialization Specification, chapter 6), for the few classes a JCEKS key
// store's secret-key entries hold: objects of serializable classes with
// default field serialization, byte arrays, strings, enum constants, and
// back-references to any of them. Anything else (another array type, a class
// with its own writeObject data, an externalizable or proxy class) is refused.

// javaObject is a deserialized object: its class and its serialized fields, by
// name. A field is a []byte (byte[]), a string, a *javaObject, a javaEnum, nil,
// or a primitive's big-endian bytes.
type javaObject struct {
	class  *javaClassDesc
	fields map[string]any
}

// javaClassDesc is a class descriptor: the class, its flags and fields, and
// its superclass's descriptor (nil for none serializable).
type javaClassDesc struct {
	name   string
	flags  byte
	fields []javaFieldDesc
	super  *javaClassDesc
}

type javaFieldDesc struct {
	typecode byte
	name     string
}

// javaEnum is an enum constant.
type javaEnum struct {
	class    *javaClassDesc
	constant string
}

// isA says the class or a superclass is name.
func (c *javaClassDesc) isA(name string) bool {
	for ; c != nil; c = c.super {
		if c.name == name {
			return true
		}
	}
	return false
}

const (
	javaStreamMagic    = 0xaced
	javaStreamVersion  = 5
	javaTCNull         = 0x70
	javaTCReference    = 0x71
	javaTCClassDesc    = 0x72
	javaTCObject       = 0x73
	javaTCString       = 0x74
	javaTCArray        = 0x75
	javaTCBlockData    = 0x77
	javaTCEndBlockData = 0x78
	javaTCBlockDataLng = 0x7a
	javaTCLongString   = 0x7c
	javaTCEnum         = 0x7e
	javaBaseWireHandle = 0x7e0000
	javaSCWriteMethod  = 0x01
	javaSCSerializable = 0x02
	javaSCExternal     = 0x04
	javaMaxDepth       = 32
)

var errJavaStreamTruncated = errors.New("serialized object: truncated")

type javaStreamReader struct {
	data    []byte
	pos     int
	handles []any
	depth   int
}

// readJavaObject reads one object stream, its magic, version and one object,
// from the start of data. It returns the object and the bytes the stream took.
func readJavaObject(data []byte) (any, int, error) {
	r := &javaStreamReader{data: data}
	magic, err := r.u16()
	if err != nil {
		return nil, 0, err
	}
	version, err := r.u16()
	if err != nil {
		return nil, 0, err
	}
	if magic != javaStreamMagic || version != javaStreamVersion {
		return nil, 0, fmt.Errorf("serialized object: bad stream header %04x %04x", magic, version)
	}
	v, err := r.content()
	if err != nil {
		return nil, 0, err
	}
	return v, r.pos, nil
}

func (r *javaStreamReader) take(n int) ([]byte, error) {
	if n < 0 || n > len(r.data)-r.pos {
		return nil, errJavaStreamTruncated
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *javaStreamReader) u8() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *javaStreamReader) u16() (int, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b)), nil
}

func (r *javaStreamReader) i32() (int, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return int(int32(binary.BigEndian.Uint32(b))), nil //nolint:gosec // a Java int
}

// utf reads a string in the stream's modified UTF-8 with a 2-byte length (or
// an 8-byte one, long). The strings a key store holds are plain UTF-8.
func (r *javaStreamReader) utf(long bool) (string, error) {
	n := 0
	if long {
		b, err := r.take(8)
		if err != nil {
			return "", err
		}
		v := binary.BigEndian.Uint64(b)
		if v > uint64(len(r.data)) {
			return "", errJavaStreamTruncated
		}
		n = int(v) //nolint:gosec // bounded by the data's length
	} else {
		var err error
		if n, err = r.u16(); err != nil {
			return "", err
		}
	}
	b, err := r.take(n)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("serialized object: a string is not UTF-8")
	}
	return string(b), nil
}

// newHandle assigns the next wire handle to v, which may be filled in later.
func (r *javaStreamReader) newHandle(v any) int {
	r.handles = append(r.handles, v)
	return len(r.handles) - 1
}

func (r *javaStreamReader) content() (any, error) {
	r.depth++
	defer func() { r.depth-- }()
	if r.depth > javaMaxDepth {
		return nil, errors.New("serialized object: nested too deep")
	}
	tc, err := r.u8()
	if err != nil {
		return nil, err
	}
	switch tc {
	case javaTCNull:
		return nil, nil
	case javaTCReference:
		h, err := r.i32()
		if err != nil {
			return nil, err
		}
		h -= javaBaseWireHandle
		if h < 0 || h >= len(r.handles) || r.handles[h] == nil {
			return nil, fmt.Errorf("serialized object: bad reference %#x", h+javaBaseWireHandle)
		}
		return r.handles[h], nil
	case javaTCString, javaTCLongString:
		s, err := r.utf(tc == javaTCLongString)
		if err != nil {
			return nil, err
		}
		r.newHandle(s)
		return s, nil
	case javaTCClassDesc:
		return r.newClassDesc()
	case javaTCArray:
		desc, err := r.classDesc()
		if err != nil {
			return nil, err
		}
		if desc == nil || desc.name != "[B" {
			return nil, errors.New("serialized object: an array other than byte[]")
		}
		h := r.newHandle(nil)
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		b, err := r.take(n)
		if err != nil {
			return nil, err
		}
		v := make([]byte, len(b)) // never nil, so a reference to it resolves
		copy(v, b)
		r.handles[h] = v
		return v, nil
	case javaTCEnum:
		desc, err := r.classDesc()
		if err != nil {
			return nil, err
		}
		h := r.newHandle(nil)
		name, err := r.content()
		if err != nil {
			return nil, err
		}
		s, ok := name.(string)
		if !ok || desc == nil {
			return nil, errors.New("serialized object: a malformed enum constant")
		}
		v := javaEnum{class: desc, constant: s}
		r.handles[h] = v
		return v, nil
	case javaTCObject:
		return r.newObject()
	}
	return nil, fmt.Errorf("serialized object: unsupported type code %#x", tc)
}

// classDesc reads a class descriptor: a new one, a reference to one, or null.
func (r *javaStreamReader) classDesc() (*javaClassDesc, error) {
	v, err := r.content()
	if err != nil || v == nil {
		return nil, err
	}
	desc, ok := v.(*javaClassDesc)
	if !ok {
		return nil, errors.New("serialized object: not a class descriptor")
	}
	return desc, nil
}

func (r *javaStreamReader) newClassDesc() (*javaClassDesc, error) {
	name, err := r.utf(false)
	if err != nil {
		return nil, err
	}
	if _, err := r.take(8); err != nil { // serialVersionUID
		return nil, err
	}
	desc := &javaClassDesc{name: name}
	r.newHandle(desc)
	if desc.flags, err = r.u8(); err != nil {
		return nil, err
	}
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	for range n {
		var f javaFieldDesc
		if f.typecode, err = r.u8(); err != nil {
			return nil, err
		}
		if f.name, err = r.utf(false); err != nil {
			return nil, err
		}
		if f.typecode == '[' || f.typecode == 'L' {
			if _, err := r.content(); err != nil { // the field's type name
				return nil, err
			}
		} else if javaPrimitiveSize(f.typecode) == 0 {
			return nil, fmt.Errorf("serialized object: field type %q", f.typecode)
		}
		desc.fields = append(desc.fields, f)
	}
	if err := r.skipAnnotation(); err != nil {
		return nil, err
	}
	if desc.super, err = r.classDesc(); err != nil {
		return nil, err
	}
	return desc, nil
}

// skipAnnotation skips block data and objects up to the end-of-block marker.
func (r *javaStreamReader) skipAnnotation() error {
	for {
		if r.pos >= len(r.data) {
			return errJavaStreamTruncated
		}
		switch r.data[r.pos] {
		case javaTCEndBlockData:
			r.pos++
			return nil
		case javaTCBlockData, javaTCBlockDataLng:
			long := r.data[r.pos] == javaTCBlockDataLng
			r.pos++
			n := 0
			var err error
			if long {
				n, err = r.i32()
			} else {
				var b byte
				b, err = r.u8()
				n = int(b)
			}
			if err != nil {
				return err
			}
			if _, err := r.take(n); err != nil {
				return err
			}
		default:
			if _, err := r.content(); err != nil {
				return err
			}
		}
	}
}

func (r *javaStreamReader) newObject() (any, error) {
	desc, err := r.classDesc()
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, errors.New("serialized object: an object with no class")
	}
	obj := &javaObject{class: desc, fields: map[string]any{}}
	r.newHandle(obj)
	// Class data runs from the topmost serializable superclass down.
	var chain []*javaClassDesc
	for c := desc; c != nil; c = c.super {
		chain = append(chain, c)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		c := chain[i]
		if c.flags&javaSCExternal != 0 || c.flags&javaSCSerializable == 0 {
			return nil, fmt.Errorf("serialized object: class %s is not plainly serializable", c.name)
		}
		for _, f := range c.fields {
			var v any
			if size := javaPrimitiveSize(f.typecode); size > 0 {
				b, err := r.take(size)
				if err != nil {
					return nil, err
				}
				v = append([]byte(nil), b...)
			} else if v, err = r.content(); err != nil {
				return nil, err
			}
			obj.fields[f.name] = v
		}
		if c.flags&javaSCWriteMethod != 0 {
			return nil, fmt.Errorf("serialized object: class %s writes its own data", c.name)
		}
	}
	return obj, nil
}

func javaPrimitiveSize(typecode byte) int {
	switch typecode {
	case 'B', 'Z':
		return 1
	case 'C', 'S':
		return 2
	case 'I', 'F':
		return 4
	case 'J', 'D':
		return 8
	}
	return 0
}

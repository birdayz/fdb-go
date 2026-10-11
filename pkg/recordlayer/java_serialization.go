package recordlayer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
)

// A reader of the Java Object Serialization stream protocol (the Java Object
// Serialization Specification, chapter 6), for exactly what a JCEKS key store's
// secret-key entries hold: objects of serializable classes with default field
// serialization and only object fields, byte arrays, strings, enum constants,
// and back-references to any of them. Anything else (a primitive field, another
// array type, class annotations, a class with its own writeObject data, an
// externalizable or proxy class, block data) is refused. The input precedes the
// key store's digest check, so nothing it holds is trusted: every length is
// bounded by the data, and class hierarchies by javaMaxClassDepth.

// javaObject is a deserialized object: its class and its serialized fields, by
// name. A field is a []byte (byte[]), a string, a *javaObject, a javaEnum, or nil.
type javaObject struct {
	class  *javaClassDesc
	fields map[string]any
}

// javaClassDesc is a class descriptor. chain is the class and its serializable
// superclasses, topmost first, set once the descriptor is complete.
type javaClassDesc struct {
	name   string
	flags  byte
	fields []string // object fields' names, in stream order
	super  *javaClassDesc
	chain  []*javaClassDesc
}

// javaEnum is an enum constant.
type javaEnum struct {
	class    *javaClassDesc
	constant string
}

// isA says the class or a superclass is name.
func (c *javaClassDesc) isA(name string) bool {
	for _, d := range c.chain {
		if d.name == name {
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
	javaTCEndBlockData = 0x78
	javaTCEnum         = 0x7e
	javaBaseWireHandle = 0x7e0000
	javaSCWriteMethod  = 0x01
	javaSCSerializable = 0x02
	javaSCExternal     = 0x04
	javaMaxDepth       = 32 // nested contents
	javaMaxClassDepth  = 8  // a class and its serializable superclasses (JCEKS needs 2)
)

var errJavaStreamTruncated = errors.New("serialized object: truncated")

type javaStreamReader struct {
	data    []byte
	pos     int
	handles []any
	depth   int
	// allow, when set, admits each class the stream names, as a
	// deserialization filter does.
	allow func(name string) bool
}

// readJavaObject reads one object stream, its magic, version and one object,
// from the start of data. It returns the object and the bytes the stream took.
func readJavaObject(data []byte, allow func(string) bool) (any, int, error) {
	r := &javaStreamReader{data: data, allow: allow}
	head, err := r.take(4)
	if err != nil {
		return nil, 0, err
	}
	if binary.BigEndian.Uint16(head) != javaStreamMagic || binary.BigEndian.Uint16(head[2:]) != javaStreamVersion {
		return nil, 0, fmt.Errorf("serialized object: bad stream header %x", head)
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

func (r *javaStreamReader) i32() (int, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return int(int32(binary.BigEndian.Uint32(b))), nil //nolint:gosec // a Java int
}

// utf reads a string as DataInput.readUTF does: a 2-byte length, then modified
// UTF-8 (UTF-16 code units in one, two or three bytes; NUL as C0 80 or 00; a
// supplementary character as two encoded surrogates). A four-byte sequence
// is malformed, as in Java. An unpaired surrogate reads as U+FFFD.
func (r *javaStreamReader) utf() (string, error) {
	lb, err := r.take(2)
	if err != nil {
		return "", err
	}
	b, err := r.take(int(binary.BigEndian.Uint16(lb)))
	if err != nil {
		return "", err
	}
	units := make([]uint16, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			units = append(units, uint16(c))
			i++
		case c&0xe0 == 0xc0 && i+1 < len(b) && b[i+1]&0xc0 == 0x80:
			units = append(units, uint16(c&0x1f)<<6|uint16(b[i+1]&0x3f))
			i += 2
		case c&0xf0 == 0xe0 && i+2 < len(b) && b[i+1]&0xc0 == 0x80 && b[i+2]&0xc0 == 0x80:
			units = append(units, uint16(c&0x0f)<<12|uint16(b[i+1]&0x3f)<<6|uint16(b[i+2]&0x3f))
			i += 3
		default:
			return "", fmt.Errorf("serialized object: malformed input around byte %d", i)
		}
	}
	return string(utf16.Decode(units)), nil
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
	case javaTCString:
		s, err := r.utf()
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
		if desc.name != "[B" {
			return nil, fmt.Errorf("serialized object: a %s array, not byte[]", desc.name)
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
		if !ok {
			return nil, errors.New("serialized object: an enum constant not named by a string")
		}
		v := javaEnum{class: desc, constant: s}
		r.handles[h] = v
		return v, nil
	case javaTCObject:
		return r.newObject()
	}
	return nil, fmt.Errorf("serialized object: unsupported type code %#x", tc)
}

// classDesc reads the class descriptor an object, array or enum names: a new
// one or a reference to a complete one. A descriptor still being read (one
// naming itself, directly or not, as its superclass) is refused.
func (r *javaStreamReader) classDesc() (*javaClassDesc, error) {
	v, err := r.content()
	if err != nil {
		return nil, err
	}
	desc, ok := v.(*javaClassDesc)
	if !ok || desc.chain == nil {
		return nil, errors.New("serialized object: not a complete class descriptor")
	}
	return desc, nil
}

func (r *javaStreamReader) newClassDesc() (*javaClassDesc, error) {
	name, err := r.utf()
	if err != nil {
		return nil, err
	}
	if r.allow != nil && name != "[B" && !r.allow(name) {
		return nil, fmt.Errorf("serialized object: class %s rejected", name)
	}
	if _, err := r.take(8); err != nil { // serialVersionUID
		return nil, err
	}
	desc := &javaClassDesc{name: name}
	r.newHandle(desc)
	if desc.flags, err = r.u8(); err != nil {
		return nil, err
	}
	nb, err := r.take(2)
	if err != nil {
		return nil, err
	}
	for range binary.BigEndian.Uint16(nb) {
		typecode, err := r.u8()
		if err != nil {
			return nil, err
		}
		field, err := r.utf()
		if err != nil {
			return nil, err
		}
		if typecode != '[' && typecode != 'L' {
			return nil, fmt.Errorf("serialized object: class %s has a primitive field %s", name, field)
		}
		if typ, err := r.content(); err != nil {
			return nil, err
		} else if _, ok := typ.(string); !ok {
			return nil, fmt.Errorf("serialized object: field %s's type is not a string", field)
		}
		desc.fields = append(desc.fields, field)
	}
	if end, err := r.u8(); err != nil {
		return nil, err
	} else if end != javaTCEndBlockData {
		return nil, fmt.Errorf("serialized object: class %s carries annotations", name)
	}
	// The superclass: none, or a complete descriptor (never this one).
	sup, err := r.content()
	if err != nil {
		return nil, err
	}
	if sup != nil {
		s, ok := sup.(*javaClassDesc)
		if !ok || s.chain == nil {
			return nil, fmt.Errorf("serialized object: class %s's superclass is not a complete class descriptor", name)
		}
		if len(s.chain) >= javaMaxClassDepth {
			return nil, fmt.Errorf("serialized object: class %s has more than %d serializable superclasses", name, javaMaxClassDepth-1)
		}
		desc.super = s
		desc.chain = append(append(make([]*javaClassDesc, 0, len(s.chain)+1), s.chain...), desc)
	} else {
		desc.chain = []*javaClassDesc{desc}
	}
	return desc, nil
}

func (r *javaStreamReader) newObject() (any, error) {
	desc, err := r.classDesc()
	if err != nil {
		return nil, err
	}
	obj := &javaObject{class: desc, fields: map[string]any{}}
	r.newHandle(obj)
	// Class data runs from the topmost serializable superclass down.
	for _, c := range desc.chain {
		if c.flags&(javaSCExternal|javaSCWriteMethod) != 0 || c.flags&javaSCSerializable == 0 {
			return nil, fmt.Errorf("serialized object: class %s is not serializable by default (flags %#x)", c.name, c.flags)
		}
		for _, f := range c.fields {
			v, err := r.content()
			if err != nil {
				return nil, err
			}
			obj.fields[f] = v
		}
	}
	return obj, nil
}

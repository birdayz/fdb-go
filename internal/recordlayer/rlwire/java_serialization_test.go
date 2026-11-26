package rlwire

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

const (
	jsHead = "aced0005"
	jsUID  = "0000000000000001"
)

func jsHex(parts ...string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(strings.Join(parts, ""), " ", ""))
	if err != nil {
		panic(err)
	}
	return b
}

func jsUTF(s string) string    { return fmt.Sprintf("%04x", len(s)) + hex.EncodeToString([]byte(s)) }
func jsString(s string) string { return "74" + jsUTF(s) }

func jsDesc(name, flags, super string, fields ...[2]string) string {
	d := "72" + jsUTF(name) + jsUID + flags + fmt.Sprintf("%04x", len(fields))
	for _, f := range fields {
		d += hex.EncodeToString([]byte{f[1][0]}) + jsUTF(f[0]) + jsString(f[1])
	}
	return d + "78" + super
}

func TestReadJavaObject_Refusals(t *testing.T) {
	t.Parallel()
	nested := jsHead + "73" + jsDesc("A", "02", "70", [2]string{"f", "LA;"})
	for range 40 {
		nested += "73" + "71007e0000"
	}
	nested += "70"
	deep := "70"
	for i := range 9 {
		deep = "72" + jsUTF(fmt.Sprintf("C%d", i)) + jsUID + "02" + "0000" + "78" + deep
	}
	for _, c := range []struct {
		name, stream, want string
		allow              func(string) bool
	}{
		{"bad header", "aced0004" + "70", "bad stream header", nil},
		{"truncated", "aced00", "truncated", nil},
		{"bad type code", jsHead + "7f", "unsupported type code", nil},
		{"bad back-reference", jsHead + "71007e0005", "bad reference", nil},
		{"nested too deep", nested, "nested too deep", nil},
		{"an int array", jsHead + "75" + jsDesc("[I", "02", "70") + "00000000", "not byte[]", nil},
		{"externalizable", jsHead + "73" + jsDesc("A", "04", "70"), "not serializable by default", nil},
		{"not serializable", jsHead + "73" + jsDesc("A", "00", "70"), "not serializable by default", nil},
		{"its own writeObject", jsHead + "73" + jsDesc("A", "03", "70"), "not serializable by default", nil},
		{"a primitive field", jsHead + "73" + "72" + jsUTF("A") + jsUID + "02" + "0001" + "49" + jsUTF("i") + "78" + "70", "primitive field", nil},
		{"class annotations", jsHead + "73" + "72" + jsUTF("A") + jsUID + "02" + "0000" + "70" + "78" + "70", "carries annotations", nil},
		{"a field type not a string", jsHead + "73" + "72" + jsUTF("A") + jsUID + "02" + "0001" + "4c" + jsUTF("f") + "70" + "78" + "70", "type is not a string", nil},
		{"bad UTF", jsHead + "74" + "0001ff", "malformed input", nil},
		{"four-byte UTF-8", jsHead + "74" + "0004f09f9880", "malformed input", nil},
		{"a negative array length", jsHead + "75" + jsDesc("[B", "02", "70") + "ffffffff", "truncated", nil},
		{"a superclass naming its subclass", jsHead + "73" + "72" + jsUTF("A") + jsUID + "02" + "0000" + "78" + "71007e0000", "not a complete class descriptor", nil},
		{"too many superclasses", jsHead + "73" + deep, "serializable superclasses", nil},
		{"a rejected class", jsHead + "73" + jsDesc("Evil", "02", "70"), "class Evil rejected", func(string) bool { return false }},
		{"an enum not named by a string", jsHead + "7e" + jsDesc("E", "12", "70") + "70", "not named by a string", nil},
	} {
		_, _, err := ReadJavaObject(jsHex(c.stream), c.allow)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func FuzzReadJavaObject(f *testing.F) {
	f.Add(jsHex(jsHead + "73" + "72" + jsUTF("A") + jsUID + "02" + "0000" + "78" + "71007e0000"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		_, n, err := ReadJavaObject(data, nil)
		if err == nil && n > len(data) {
			t.Fatalf("read %d of %d bytes", n, len(data))
		}
	})
}

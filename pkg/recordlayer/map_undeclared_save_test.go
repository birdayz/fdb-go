package recordlayer

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A DynamicMessage map value holding a closed enum's undeclared number is
// written as Java writes it: Java reads the entry keeping the number in the
// entry's own unknown fields and writes it back after the value (measured
// against the JVM by the conformance spec "a record Go holds with an undeclared
// number is saved, updated and deleted as Java reads it": key, default value,
// then the number). A dynamicpb map has no per-entry unknown fields, so Go's
// save dropped the number. These pin the paths the spec does not reach: a map
// inside a message the record did not store before, the load-then-save of the
// entry as Java wrote it, and a value changed to a declared one.
func TestMapValueUndeclaredNumberIsWrittenAsJavaWritesIt(t *testing.T) {
	t.Parallel()
	md := javaDecodeFile(t).Messages().ByName("M")
	rt := testRecordType(md)
	x := newClosedEnumReach(false, md)
	rt.reachesClosedEnum, rt.closedEnumReach = x.reaches(md), x
	me, child := md.Fields().ByName("me"), md.Fields().ByName("child")
	key := protoreflect.ValueOfString("k").MapKey()

	// The entry for key "k" as Java writes it: key, the enum's default (F0,
	// 0), then the undeclared number as the entry's unknown field.
	javaEntry := func(n uint64) []byte {
		b := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k")
		b = protowire.AppendVarint(protowire.AppendTag(b, 2, protowire.VarintType), 0)
		return protowire.AppendVarint(protowire.AppendTag(b, 2, protowire.VarintType), n)
	}
	entries := func(t *testing.T, inner []byte) [][]byte {
		t.Helper()
		var out [][]byte
		for len(inner) > 0 {
			n, typ, k := protowire.ConsumeTag(inner)
			size := protowire.ConsumeFieldValue(n, typ, inner[k:])
			if k < 0 || size < 0 {
				t.Fatalf("bad bytes %x", inner)
			}
			if n == me.Number() {
				body, _ := protowire.ConsumeBytes(inner[k : k+size])
				out = append(out, body)
			}
			inner = inner[k+size:]
		}
		return out
	}
	save := func(t *testing.T, msg proto.Message, prior []byte) (proto.Message, []byte) {
		t.Helper()
		read, write := rt.asJavaForSave(msg)
		data, err := serializeUnionOver(write, rt, prior)
		if err != nil {
			t.Fatal(err)
		}
		return read, unionInner(data, 1)
	}
	held := func() *dynamicpb.Message {
		m := dynamicpb.NewMessage(md)
		m.Mutable(me).Map().Set(key, protoreflect.ValueOfEnum(9))
		return m
	}

	t.Run("a new record", func(t *testing.T) {
		t.Parallel()
		msg := held()
		read, inner := save(t, msg, nil)
		if got := entries(t, inner); len(got) != 1 || !bytes.Equal(got[0], javaEntry(9)) {
			t.Fatalf("entries %x, want [%x]", got, javaEntry(9))
		}
		if v := read.ProtoReflect().Get(me).Map().Get(key).Enum(); v != 0 {
			t.Errorf("the saved record reads %d, want the default as Java reads it", v)
		}
		if v := msg.Get(me).Map().Get(key).Enum(); v != 9 {
			t.Errorf("the caller's message was changed: %d", v)
		}
	})

	t.Run("a map in a message the record did not store before", func(t *testing.T) {
		t.Parallel()
		// The prior record has no child; the update adds one whose map holds
		// the number.
		_, prior := save(t, dynamicpb.NewMessage(md), nil)
		msg := dynamicpb.NewMessage(md)
		msg.Mutable(child).Message().Mutable(me).Map().Set(key, protoreflect.ValueOfEnum(9))
		_, inner := save(t, msg, prior)
		var childBody []byte
		for rest := inner; len(rest) > 0; {
			n, typ, k := protowire.ConsumeTag(rest)
			size := protowire.ConsumeFieldValue(n, typ, rest[k:])
			if n == child.Number() {
				childBody, _ = protowire.ConsumeBytes(rest[k : k+size])
			}
			rest = rest[k+size:]
		}
		if got := entries(t, childBody); len(got) != 1 || !bytes.Equal(got[0], javaEntry(9)) {
			t.Fatalf("the child's entries %x, want [%x]", got, javaEntry(9))
		}
	})

	t.Run("a load and save of the entry keeps the number", func(t *testing.T) {
		t.Parallel()
		read, stored := save(t, held(), nil)
		// read is the record as a load reads the stored bytes: the default.
		_, again := save(t, read, stored)
		if !bytes.Equal(again, stored) {
			t.Fatalf("resaved %x, want the stored %x", again, stored)
		}
	})

	t.Run("a value changed to a declared one drops the number", func(t *testing.T) {
		t.Parallel()
		read, stored := save(t, held(), nil)
		changed := proto.Clone(read).(*dynamicpb.Message)
		changed.Mutable(me).Map().Set(key, protoreflect.ValueOfEnum(2))
		_, inner := save(t, changed, stored)
		want := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k")
		want = protowire.AppendVarint(protowire.AppendTag(want, 2, protowire.VarintType), 2)
		if got := entries(t, inner); len(got) != 1 || !bytes.Equal(got[0], want) {
			t.Fatalf("entries %x, want [%x]", got, want)
		}
	})

	t.Run("the same undeclared number held again is unchanged", func(t *testing.T) {
		t.Parallel()
		_, stored := save(t, held(), nil)
		_, inner := save(t, held(), stored)
		if !bytes.Equal(inner, stored) {
			t.Fatalf("resaved %x, want the stored %x", inner, stored)
		}
	})
}

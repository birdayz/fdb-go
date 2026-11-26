package rlcrypto

import (
	"bytes"
	mathrand "math/rand/v2"
	"os"
	"os/exec"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestPrefixIsAProtobufVarint(t *testing.T) {
	t.Parallel()
	r := mathrand.New(mathrand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		key := int32(r.Uint32()) //nolint:gosec
		st := &TransformState{Data: []byte("x"), Encrypted: true, Compressed: i%2 == 0, KeyNumber: key}
		out := EncodeTransformPrefix(st)
		want := uint64(TransformPrefixEncrypted) | uint64(key)<<TransformKeyShift //nolint:gosec
		if st.Compressed {
			want |= TransformPrefixCompressed
		}
		if got := protowire.AppendVarint(nil, want); !bytes.Equal(out[:len(got)], got) {
			t.Fatalf("key %d: prefix %x, want the varint %x", key, out[:len(got)], got)
		}
		back, prefixed, err := DecodeTransformPrefix(out)
		if err != nil || !prefixed || back.KeyNumber != key || back.Compressed != st.Compressed || !back.Encrypted ||
			string(back.Data) != "x" {
			t.Fatalf("key %d decoded as %+v, %t, %v", key, back, prefixed, err)
		}
	}
}

func TestDecodeTransformPrefixDoesNotAllocate(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const bench = "BenchmarkDecodeTransformPrefixAllocations"
	output, err := exec.CommandContext(t.Context(), executable,
		"-test.run=^$", "-test.bench=^"+bench+"$", "-test.benchtime=1x").CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(bench)) {
		t.Fatalf("allocation benchmark: %v\n%s", err, output)
	}
}

func BenchmarkDecodeTransformPrefixAllocations(b *testing.B) {
	stored := append(protowire.AppendVarint(nil, TransformPrefixCompressed), 'x')
	var st TransformState
	var prefixed bool
	allocations := testing.AllocsPerRun(100, func() {
		st, prefixed, _ = DecodeTransformPrefix(stored)
	})
	if !prefixed || !st.Compressed || string(st.Data) != "x" {
		b.Fatalf("decoded %+v, prefixed=%t", st, prefixed)
	}
	if allocations != 0 {
		b.Fatalf("DecodeTransformPrefix allocated %.0f times per record", allocations)
	}
}

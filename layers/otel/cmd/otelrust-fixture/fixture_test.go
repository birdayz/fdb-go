package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"google.golang.org/protobuf/proto"
)

func TestPinnedRustCompatibilityFixtures(t *testing.T) {
	t.Parallel()
	vectors, descriptors, err := fixture()
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 9 {
		t.Fatalf("expected nine compatibility vectors, got %d", len(vectors))
	}
	for _, v := range vectors {
		if len(v.Keys) != 8 || len(v.OTLP) == 0 || len(v.Row) == 0 || v.PK == "" {
			t.Fatalf("incomplete vector %s", v.Name)
		}
		if v.Name != "fidelity" {
			keys := 0
			for _, index := range v.Keys {
				keys += len(index)
			}
			if keys != 10 {
				t.Fatalf("%s has %d indexes, want 8 attribute + 2 intrinsic", v.Name, keys)
			}
		}
	}
	jsonBytes, err := json.MarshalIndent(vectors, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	descriptorBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	r, err := runfiles.New()
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string][]byte{"vectors.json": append(jsonBytes, '\n'), "descriptors.pb": descriptorBytes} {
		path, err := r.Rlocation("_main/layers/otel/rust/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, expected) {
			t.Fatalf("%s drift: regenerate with otelrust-fixture into a fresh directory and inspect", name)
		}
	}
}

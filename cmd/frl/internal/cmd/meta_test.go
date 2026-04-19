package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func TestWriteTypesList_RendersFixture(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t) // shared fixture from index_test.go

	var buf bytes.Buffer
	if err := writeTypesList(&buf, md); err != nil {
		t.Fatalf("writeTypesList: %v", err)
	}
	out := buf.String()

	// Header + one line per type. Demo proto has 3 types.
	for _, want := range []string{
		"NAME",
		"PRIMARY KEY",
		"SINCE VERSION",
		"Order",
		"Customer",
		"TypedRecord",
		"order_id",
		"customer_id",
		"id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("writeTypesList output missing %q\nfull output:\n%s", want, out)
		}
	}

	// Output rows must be sorted (Customer < Order < TypedRecord).
	customerIdx := strings.Index(out, "Customer")
	orderIdx := strings.Index(out, "Order")
	typedIdx := strings.Index(out, "TypedRecord")
	if !(customerIdx < orderIdx && orderIdx < typedIdx) {
		t.Errorf("rows not alphabetically sorted:\n%s", out)
	}
}

// writeDemoMetaFile exports the shared demo metadata to a temp file via
// WriteRecordMetaData. Used by validate/evolve-check tests so they don't
// all re-hand-roll the dumper.
func writeDemoMetaFile(t *testing.T, version int32) string {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	if version > 0 {
		builder.SetVersion(int(version))
	}
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build demo metadata: %v", err)
	}
	path := filepath.Join(t.TempDir(), "meta.pb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := recordlayer.WriteRecordMetaData(md, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestMetaValidate_Succeeds(t *testing.T) {
	t.Parallel()
	path := writeDemoMetaFile(t, 0)
	c := newMetaValidateCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--file", path})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "ok:") {
		t.Errorf("output missing 'ok:' line:\n%s", out.String())
	}
}

func TestMetaValidate_RequiresFile(t *testing.T) {
	t.Parallel()
	c := newMetaValidateCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error when --file omitted")
	}
}

func TestMetaEvolveCheck_SameVersionRejected(t *testing.T) {
	t.Parallel()
	old := writeDemoMetaFile(t, 0)
	newer := writeDemoMetaFile(t, 0)
	c := newMetaEvolveCheckCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--old", old, "--new", newer})
	// Same version → MetaDataEvolutionValidator rejects (newer must advance).
	err := c.Execute()
	if err == nil {
		t.Fatal("expected evolve-check to reject same-version evolution, got nil")
	}
	if !strings.Contains(err.Error(), "newer version") {
		t.Errorf("error text %q doesn't mention the version-advance requirement", err.Error())
	}
}

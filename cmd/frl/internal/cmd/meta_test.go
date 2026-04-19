package cmd

import (
	"bytes"
	"strings"
	"testing"
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

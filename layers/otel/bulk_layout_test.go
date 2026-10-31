package otel

import (
	"encoding/json"
	"testing"
)

func TestPostingLayoutAndBulkOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Backend, c.SaveMode = "fdb", "batch"
	plain := manifest(c, "ready")
	wire, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"", "json", "tuple"} {
		d := c
		d.PostingValueFormat = format
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		m := manifest(d, "ready")
		if err := checkManifest(d, m, true); err != nil {
			t.Fatal(err)
		}
		if corpusIdentity(d) != corpusIdentity(c) {
			t.Fatal("covering encoding changed source identity")
		}
		_, index, err := metadata(d)
		if err != nil {
			t.Fatal(err)
		}
		if format == "tuple" {
			if index.Options["value_format"] != "tuple" {
				t.Fatal("tuple choice not bound to index metadata")
			}
			requireOtelKind(t, checkManifest(c, m, true), "ownership")
			requireOtelKind(t, checkManifest(d, plain, true), "ownership")
		} else {
			if _, changed := index.Options["value_format"]; changed {
				t.Fatal("legacy JSON metadata changed")
			}
			got, err := json.Marshal(m)
			if err != nil || string(got) != string(wire) {
				t.Fatal("legacy JSON ownership bytes changed")
			}
		}
		// Conflict coalescing changes execution, not source/layout identity.
		d.BulkIndexConflicts = true
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if manifest(d, "ready") != m {
			t.Fatal("bulk execution choice changed stored layout identity")
		}
	}
	for _, backend := range []string{"ch-map", "ch-attrs"} {
		d := c
		d.Backend, d.PostingValueFormat = backend, "tuple"
		if d.Validate() == nil {
			t.Fatalf("accepted tuple postings for %s", backend)
		}
		d.PostingValueFormat, d.BulkIndexConflicts = "json", true
		if d.Validate() == nil {
			t.Fatalf("accepted bulk Record Layer saves for %s", backend)
		}
	}
	for _, mode := range []string{"serial", "pipelined"} {
		d := c
		d.SaveMode, d.BulkIndexConflicts = mode, true
		if d.Validate() == nil {
			t.Fatalf("silently ignored bulk option for %s", mode)
		}
	}
	d := c
	d.PostingValueFormat = "unknown"
	if d.Validate() == nil {
		t.Fatal("accepted unknown posting format")
	}
	plain.PostingValueFormat = "unknown"
	requireOtelKind(t, checkManifest(c, plain, true), "ownership")
}

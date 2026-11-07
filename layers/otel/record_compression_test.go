package otel

import "testing"

// The record compression modes of the sql backend's OTEL_V1 stores: Java-
// readable deflate only (D2). They leave the source corpus unchanged, and
// the ClickHouse backends refuse them.
func TestRecordCompressionConfigAndOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Backend = "sql"
	for _, mode := range []string{"", "none", "zlib-1", "zlib-default"} {
		t.Run(mode, func(t *testing.T) {
			d := c
			d.RecordCompression = mode
			if err := d.Validate(); err != nil {
				t.Fatal(err)
			}
			serializer, err := recordSerializer(mode)
			if err != nil || (serializer == nil) != (recordCompressionMode(mode) == "") {
				t.Fatalf("serializer %q: %v %v", mode, serializer, err)
			}
			if corpusIdentity(c) != corpusIdentity(d) {
				t.Fatal("storage compression changed the source corpus")
			}
		})
	}
	for _, mode := range []string{"brotli", "brotli-0", "brotli-1", "brotli-3", "brotli-4", "brotli-11", "zlib-9", "unknown"} {
		d := c
		d.RecordCompression = mode
		if d.Validate() == nil {
			t.Fatalf("accepted compression mode %q", mode)
		}
		if _, err := recordSerializer(mode); err == nil {
			t.Fatalf("constructed compression mode %q", mode)
		}
	}
	for _, backend := range []string{"ch-map", "ch-attrs"} {
		d := c
		d.Backend, d.RecordCompression = backend, "zlib-1"
		if d.Validate() == nil {
			t.Fatalf("accepted Record Layer compression for %s", backend)
		}
	}
}

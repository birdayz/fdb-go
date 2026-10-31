package otel

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
)

func TestRecordCompressionConfigAndOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Backend = "fdb"
	original := manifest(c, "ready")
	originalWire, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if original.RecordCompression != "" {
		t.Fatal("plain layout acquired a compression identity")
	}
	for _, mode := range []string{"", "none", "zlib-1", "brotli-0", "brotli-1", "brotli-3", "brotli-4"} {
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
			m := manifest(d, "ready")
			if corpusIdentity(c) != corpusIdentity(d) {
				t.Fatal("storage compression changed the source corpus")
			}
			if err := checkManifest(d, m, true); err != nil {
				t.Fatal(err)
			}
			if recordCompressionMode(mode) == "" {
				wire, err := json.Marshal(m)
				if err != nil || string(wire) != string(originalWire) {
					t.Fatal("uncompressed manifest bytes changed")
				}
			} else {
				requireOtelKind(t, checkManifest(d, original, true), "ownership")
				requireOtelKind(t, checkManifest(c, m, true), "ownership")
				for _, other := range []string{"zlib-1", "brotli-0", "brotli-1", "brotli-3", "brotli-4"} {
					if other != mode {
						wrong := d
						wrong.RecordCompression = other
						requireOtelKind(t, checkManifest(wrong, m, true), "ownership")
					}
				}
			}
		})
	}
	for _, mode := range []string{"brotli", "brotli-11", "zlib-9", "unknown"} {
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
		d.Backend, d.RecordCompression = backend, "brotli-1"
		if d.Validate() == nil {
			t.Fatalf("accepted Record Layer compression for %s", backend)
		}
	}
}

// This is a bounded byte/correctness experiment, not a sustained-rate test.
func testOtelRecordCompression(t *testing.T, ctx context.Context, cluster, order string) {
	var plainPostings map[string]string
	var plainLogical int64
	for _, arm := range []struct {
		name, compression, values string
		bulk, shortHashes         bool
	}{
		{"plain", "none", "json", false, false},
		{"zlib1", "zlib-1", "json", false, false},
		{"brotli0", "brotli-0", "json", false, false},
		{"brotli1", "brotli-1", "json", false, false},
		{"brotli3", "brotli-3", "json", false, false},
		{"brotli4", "brotli-4", "json", false, false},
		{"tuple", "none", "tuple", false, false},
		{"tuple_brotli1", "brotli-1", "tuple", false, false},
		{"tuple_brotli1_bulk", "brotli-1", "tuple", true, false},
		{"tuple_brotli1_bulk_hash8", "brotli-1", "tuple", true, true},
		{"tuple_brotli4_bulk_hash8", "brotli-4", "tuple", true, true},
		{"tuple_zlib1_bulk_hash8", "zlib-1", "tuple", true, true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			mode := arm.compression
			c := testConfig()
			c.Backend, c.Order, c.SaveMode = "fdb", order, "batch"
			c.Count, c.Tenants, c.Attributes, c.Batch = 256, 1, 8, 64
			c.RecordCompression, c.Diagnostics = mode, true
			c.PostingValueFormat, c.BulkIndexConflicts = arm.values, arm.bulk
			if arm.shortHashes {
				c.HashBytes = 8
			}
			b := openOtelFixture(t, ctx, cluster, c, false)
			started := time.Now()
			progress, err := b.Ingest(ctx)
			elapsed := time.Since(started)
			if err != nil || progress.Rows != int64(c.Count) || progress.Attributes != int64(c.Count*8) {
				t.Fatalf("compressed ingestion: %+v %v", progress, err)
			}
			audit, err := b.Audit(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for name, count := range map[string]int64{"records": 256, "eq_secondary": 2048, spanIndexName: 256, timeIndexName: 256, "owned_total": 2818} {
				if audit.Structures[name].Keys != count {
					t.Fatalf("%s census %+v, want %d", name, audit.Structures[name], count)
				}
			}
			if err := AuditQueries(ctx, b, b.cfg); err != nil {
				t.Fatal(err)
			}
			sources := make([]Span, c.Count)
			for i := range sources {
				sources[i] = Generate(c, i)
			}
			assertOtelCommittedPostings(t, ctx, b, sources)
			// Remove only the fresh run's common prefix before comparing bytes.
			postings := otelSnapshot(t, ctx, b, otelSecondary(b))
			normalized := make(map[string]string, len(postings))
			prefix := otelSecondary(b).Bytes()
			for key, value := range postings {
				if arm.values == "tuple" {
					decoded, err := decodeCoveringValue([]byte(value), "tuple")
					if err != nil {
						t.Fatal(err)
					}
					wire, err := json.Marshal(decoded)
					if err != nil {
						t.Fatal(err)
					}
					value = string(wire)
				}
				normalized[key[len(prefix):]] = value
			}
			if arm.name == "plain" {
				plainPostings, plainLogical = normalized, progress.LogicalPayloadBytes
			} else if (!arm.shortHashes && !reflect.DeepEqual(plainPostings, normalized)) || plainLogical != progress.LogicalPayloadBytes {
				t.Fatal("storage encoding changed source bytes or complete covering memberships")
			}
			// A differently configured reader is rejected by ownership before use.
			wrong := b.cfg
			wrong.RecordCompression = "none"
			if mode == "none" {
				wrong.RecordCompression = "brotli-1"
			}
			other, err := OpenFDBContext(ctx, wrong, cluster)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			_, err = other.Trace(ctx, Generate(c, 0).Trace)
			requireOtelKind(t, err, "ownership")
			// All primary loads still go through the Record Layer serializer.
			otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
				for _, ordinal := range []int{0, 127, 255} {
					s := Generate(c, ordinal)
					r, err := store.LoadRecord(otelPK(s))
					if err != nil {
						return err
					}
					if r == nil {
						t.Fatalf("missing primary %d", ordinal)
					}
					decoded, err := decodeRecord(r.Record)
					if err != nil || !equalSpan(decoded, s) {
						t.Fatalf("decoded primary %d: %+v %v", ordinal, decoded, err)
					}
				}
				return nil
			})
			t.Logf("record_compression_bytes arm=%s mode=%s values=%s bulk=%t hash_bytes=%d order=%s rows=%d primary_value_bytes=%d posting_key_bytes=%d posting_value_bytes=%d owned_key_bytes=%d owned_value_bytes=%d approximate_transaction_bytes=%d ingest_elapsed_ns=%d; bounded integration sample, not sustained throughput", arm.name, mode, arm.values, arm.bulk, c.HashBytes, order, progress.Rows, audit.Structures["records"].ValueBytes, audit.Structures["eq_secondary"].KeyBytes, audit.Structures["eq_secondary"].ValueBytes, audit.Structures["owned_total"].KeyBytes, audit.Structures["owned_total"].ValueBytes, progress.Diagnostics.ApproximateMutationBytes, elapsed.Nanoseconds())
		})
	}
}

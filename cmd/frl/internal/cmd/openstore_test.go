package cmd

import (
	"errors"
	"strings"
	"testing"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/pkg/recordlayer"
)

func TestLookupRecordType_Found(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	rt, err := lookupRecordType(md, "Order")
	if err != nil {
		t.Fatalf("lookupRecordType(Order): %v", err)
	}
	if rt == nil || rt.Name != "Order" {
		t.Errorf("got rt = %v; want Name=Order", rt)
	}
}

func TestLookupRecordType_Missing(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	_, err := lookupRecordType(md, "NotReal")
	if err == nil {
		t.Fatal("expected error for missing type")
	}
	// Must echo the offending name, a "not found" sentinel-ish token,
	// and every real type so the operator can fix the typo from the
	// terminal. Alphabetical order is already tested elsewhere; here
	// we just confirm all three names appear.
	for _, want := range []string{"NotReal", "not found", "Customer", "Order", "TypedRecord"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q; missing %q", err.Error(), want)
		}
	}
}

func TestValidateRecordType_WrapsLookup(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	if err := validateRecordType(md, "Order"); err != nil {
		t.Errorf("valid type returned error: %v", err)
	}
	if err := validateRecordType(md, "Bogus"); err == nil {
		t.Error("expected error for invalid type")
	}
}

func TestLookupIndex_Found(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	idx, err := lookupIndex(md, "Order$price")
	if err != nil {
		t.Fatalf("lookupIndex: %v", err)
	}
	if idx == nil || idx.Name != "Order$price" {
		t.Errorf("got idx = %v; want Name=Order$price", idx)
	}
}

func TestLookupIndex_MissingListsAllAvailable(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
		b.AddIndex("Customer", recordlayer.NewIndex("Customer$name", recordlayer.Field("name")))
	})
	_, err := lookupIndex(md, "Order$missing")
	if err == nil {
		t.Fatal("expected error")
	}
	// Alphabetical listing is already tested in meta_diff; here we just
	// verify both indexes appear somewhere in the error.
	for _, want := range []string{"Order$missing", "not found", "Customer$name", "Order$price"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestResolveMetaSourceFile_OverrideShortCircuits — when the caller
// already resolved a meta.Source (via --meta-file), the helper must
// return it verbatim without consulting the context's metadata.
func TestResolveMetaSourceFile_OverrideShortCircuits(t *testing.T) {
	t.Parallel()
	override := &meta.FileSource{Path: "/tmp/whatever.pb"}
	// Context has no metadata at all — would error if consulted.
	cfgCtx := &configv1.Context{Name: "empty"}
	got, err := resolveMetaSourceFile(cfgCtx, override)
	if err != nil {
		t.Fatalf("override path should short-circuit: %v", err)
	}
	if got != override {
		t.Errorf("got %v; want the passed-in override", got)
	}
}

// TestResolveMetaSourceFile_MissingSourceWrappedWithContext confirms
// that an empty context produces an ErrMissingSource-wrapping error
// that mentions the context name — so operators with multiple contexts
// know which one is broken.
func TestResolveMetaSourceFile_MissingSourceWrappedWithContext(t *testing.T) {
	t.Parallel()
	cfgCtx := &configv1.Context{Name: "local-dev"}
	_, err := resolveMetaSourceFile(cfgCtx, nil)
	if err == nil {
		t.Fatal("expected ErrMissingSource")
	}
	if !errors.Is(err, meta.ErrMissingSource) {
		t.Errorf("error %v should unwrap to ErrMissingSource", err)
	}
	if !strings.Contains(err.Error(), "local-dev") {
		t.Errorf("error %v should name the context", err)
	}
}

// TestResolveMetaSourceFile_FDBStoreWrappedWithContext — same dance
// for the FDB-store-unsupported sentinel. Confirms callers via
// errors.Is can tell "this command is file-only" apart from
// "context has no metadata at all".
func TestResolveMetaSourceFile_FDBStoreWrappedWithContext(t *testing.T) {
	t.Parallel()
	cfgCtx := &configv1.Context{
		Name: "prod",
		Metadata: &configv1.MetadataSource{
			Source: &configv1.MetadataSource_MetaStoreKeyspace{
				MetaStoreKeyspace: "/myapp/_meta",
			},
		},
	}
	_, err := resolveMetaSourceFile(cfgCtx, nil)
	if err == nil {
		t.Fatal("expected ErrFDBStoreNotAvailable")
	}
	if !errors.Is(err, meta.ErrFDBStoreNotAvailable) {
		t.Errorf("error %v should unwrap to ErrFDBStoreNotAvailable", err)
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error %v should name the context", err)
	}
}

// storeAddressFlags mode rules: --database/--schema come as a pair and
// don't mix with --meta-file.
func TestStoreAddressFlags_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		f       storeAddressFlags
		wantErr string
	}{
		{"neither", storeAddressFlags{}, ""},
		{"both relational", storeAddressFlags{database: "/d", schema: "s"}, ""},
		{"database only", storeAddressFlags{database: "/d"}, "both"},
		{"schema only", storeAddressFlags{schema: "s"}, "both"},
		{"relational plus meta-file", storeAddressFlags{database: "/d", schema: "s", metaFile: "m.pb"}, "conflicting"},
		{"meta-file only", storeAddressFlags{metaFile: "m.pb"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.f.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validate() = %v; want substring %q", err, tc.wantErr)
			}
		})
	}
}

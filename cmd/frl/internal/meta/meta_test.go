package meta

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// sampleMetaDataBytes returns a minimal but valid serialized MetaData proto
// built from the conformance Records descriptor. Used by FileSource tests.
func sampleMetaDataBytes(t *testing.T) []byte {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	meta, err := builder.Build()
	if err != nil {
		t.Fatalf("build sample metadata: %v", err)
	}
	var buf bytes.Buffer
	if err := recordlayer.WriteRecordMetaData(meta, &buf); err != nil {
		t.Fatalf("write sample metadata: %v", err)
	}
	return buf.Bytes()
}

func TestFileSource_LoadValid(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "meta.pb")
	if err := os.WriteFile(path, sampleMetaDataBytes(t), 0o600); err != nil {
		t.Fatalf("write meta.pb: %v", err)
	}

	src := &FileSource{Path: path}
	if src.Name() == "" || !containsPath(src.Name(), path) {
		t.Errorf("Name() = %q; want it to contain %q", src.Name(), path)
	}

	meta, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta == nil {
		t.Fatal("Load returned nil metadata")
	}
}

func TestFileSource_MissingFile(t *testing.T) {
	t.Parallel()
	src := &FileSource{Path: filepath.Join(t.TempDir(), "does-not-exist.pb")}
	_, err := src.Load(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v; want errors.Is os.ErrNotExist", err)
	}
}

func TestFileSource_EmptyFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "empty.pb")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := (&FileSource{Path: path}).Load(context.Background())
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestFileSource_CorruptProto(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "corrupt.pb")
	// Clearly-not-a-proto bytes — proto.Unmarshal rejects arbitrary garbage.
	if err := os.WriteFile(path, []byte{0xff, 0xff, 0xff, 0xff, 0xff}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := (&FileSource{Path: path}).Load(context.Background())
	if err == nil {
		t.Fatal("expected error for corrupt proto, got nil")
	}
}

func TestFileSource_MetaDataMissingRecords(t *testing.T) {
	t.Parallel()
	// Serialize a MetaData proto with no `records` field — simulates an
	// incremental update or partial dump. frl must reject it with a clear
	// operator message pointing at the correct app-side fix.
	bad := &gen.MetaData{Version: proto.Int32(1)}
	raw, err := proto.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad metadata: %v", err)
	}
	path := filepath.Join(t.TempDir(), "incomplete.pb")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = (&FileSource{Path: path}).Load(context.Background())
	if err == nil {
		t.Fatal("expected error for MetaData without records, got nil")
	}
	// Accept any error that mentions the requirement so callers see guidance.
	if !containsAny(err.Error(), "records is empty", "embedded proto descriptors") {
		t.Errorf("error text %q doesn't mention the missing-descriptors requirement", err.Error())
	}
}

func TestFromContext_NoMetadataSource(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{Name: "bare"} // no metadata field
	_, err := FromContext(ctx, nil, nil)
	if !errors.Is(err, ErrMissingSource) {
		t.Errorf("error = %v; want errors.Is ErrMissingSource", err)
	}
}

func TestFromContext_EmptyMetaFile(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{
		Name: "ctx",
		Metadata: &configv1.MetadataSource{
			Source: &configv1.MetadataSource_MetaFile{MetaFile: ""},
		},
	}
	_, err := FromContext(ctx, nil, nil)
	if err == nil {
		t.Fatal("expected error for empty meta_file, got nil")
	}
}

func TestFromContext_MetaFileOK(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{
		Name: "ctx",
		Metadata: &configv1.MetadataSource{
			Source: &configv1.MetadataSource_MetaFile{MetaFile: "/etc/app/meta.pb"},
		},
	}
	src, err := FromContext(ctx, nil, nil)
	if err != nil {
		t.Fatalf("FromContext: %v", err)
	}
	fs, ok := src.(*FileSource)
	if !ok {
		t.Fatalf("source type = %T, want *FileSource", src)
	}
	if fs.Path != "/etc/app/meta.pb" {
		t.Errorf("Path = %q; want /etc/app/meta.pb", fs.Path)
	}
}

func TestFromContext_MetaStoreKeyspaceRequiresDB(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{
		Name: "ctx",
		Metadata: &configv1.MetadataSource{
			Source: &configv1.MetadataSource_MetaStoreKeyspace{MetaStoreKeyspace: "/app/_meta"},
		},
	}
	// Both the nil-DB and nil-keyspaceResolver paths must surface as the
	// same sentinel so callers can handle "FDB-store not available here"
	// uniformly with errors.Is. Operators otherwise see different
	// messages for semantically identical failures.
	_, err := FromContext(ctx, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil DB on fdb_store source, got nil")
	}
	if !errors.Is(err, ErrFDBStoreNotAvailable) {
		t.Errorf("nil DB: error = %v; want errors.Is ErrFDBStoreNotAvailable", err)
	}
	// Same sentinel when the keyspace resolver is the nil piece.
	db := &recordlayer.FDBDatabase{}
	_, err = FromContext(ctx, db, nil)
	if err == nil {
		t.Fatal("expected error for nil keyspaceResolver on fdb_store source")
	}
	if !errors.Is(err, ErrFDBStoreNotAvailable) {
		t.Errorf("nil resolver: error = %v; want errors.Is ErrFDBStoreNotAvailable", err)
	}
}

func containsPath(s, substring string) bool { return bytes.Contains([]byte(s), []byte(substring)) }
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}

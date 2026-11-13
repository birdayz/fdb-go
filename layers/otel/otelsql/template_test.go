package otelsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
)

// Record type keys are table positions and lead every record key (and end
// every index key), so a table added or left unindexed must not move spans
// off key 0, the one-byte encoding every span key pays.
func TestTemplateRecordTypeKeys(t *testing.T) {
	t.Parallel()
	md := templateMetaData(t)
	want := map[string]int64{"SPANS": 0, "LOGS": 1, "MESSAGE_TRACES": 2, "STORE_META": 3, "MESSAGES": 4}
	if len(md.RecordTypes()) != len(want) {
		t.Fatalf("%d tables, want %d", len(md.RecordTypes()), len(want))
	}
	for name, key := range want {
		rt := md.GetRecordType(name)
		if rt == nil {
			t.Fatalf("no table %s", name)
		}
		if got, ok := rt.GetRecordTypeKey().(int64); !ok || got != key {
			t.Errorf("%s record type key %v, want %d", name, rt.GetRecordTypeKey(), key)
		}
		if len(rt.GetIndexes()) == 0 {
			t.Errorf("%s has no index: the DDL front end would order it first", name)
		}
	}
}

// TemplateDDL is TemplateBody (which Java builds, the rfc204 golden) plus the
// overlay's one TEXT index, appended last: the same records descriptor and
// the same indexes and versions otherwise, so the golden covers the store
// layout of every table.
func TestTemplateOverlay(t *testing.T) {
	t.Parallel()
	if strings.Contains(strings.ToUpper(TemplateBody), "FULLTEXT") {
		t.Fatal("TemplateBody has a Go-only FULLTEXT index")
	}
	body, err := embedded.BuildSchemaTemplateFromDDL("CREATE SCHEMA TEMPLATE " + TemplateName + " " + TemplateBody)
	if err != nil {
		t.Fatal(err)
	}
	full := templateMetaData(t)
	b, f := body.Underlying(), full
	if !proto.Equal(protodesc.ToFileDescriptorProto(b.GetUnionDescriptor().ParentFile()), protodesc.ToFileDescriptorProto(f.GetUnionDescriptor().ParentFile())) {
		t.Fatal("the overlay changes the records descriptor")
	}
	bp, err := b.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	fp, err := f.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if len(bp.GetIndexes()) != 20 || len(fp.GetIndexes()) != 21 || bp.GetVersion() != 21 || fp.GetVersion() != 22 {
		t.Fatalf("indexes %d/%d, versions %d/%d", len(bp.GetIndexes()), len(fp.GetIndexes()), bp.GetVersion(), fp.GetVersion())
	}
	byName := map[string]*gen.Index{}
	for _, idx := range fp.GetIndexes() {
		byName[idx.GetName()] = idx
	}
	for _, idx := range bp.GetIndexes() {
		if !proto.Equal(idx, byName[idx.GetName()]) {
			t.Fatalf("index %s differs under the overlay: %v vs %v", idx.GetName(), idx, byName[idx.GetName()])
		}
	}
	text := byName["MESSAGES_TEXT"]
	opts := f.GetIndex("MESSAGES_TEXT").Options
	// No tokenizer option: the default tokenizer at version 0, which Java has.
	_, named := opts[recordlayer.IndexOptionTextTokenizerName]
	_, versioned := opts[recordlayer.IndexOptionTextTokenizerVersion]
	if text.GetAddedVersion() != 22 || text.GetType() != recordlayer.IndexTypeText || named || versioned || strings.Contains(TemplateDDL(), MessageTokenizerName) {
		t.Fatalf("overlay index %v, options %v", text, opts)
	}
	if f.GetRecordType("MESSAGES") == nil || len(f.GetRecordType("MESSAGES").GetIndexes()) != 3 {
		t.Fatal("MESSAGES_TEXT is not a MESSAGES index")
	}
}

func TestTenantPaths(t *testing.T) {
	t.Parallel()
	keyspace.RegisterDomainIfNotExists("FRL")
	path, schema, err := tenantPaths("/frl/tenant_42", "select")
	if err != nil || path != "/FRL/TENANT_42" || schema != "/FRL/TENANT_42/SELECT" {
		t.Fatalf("tenantPaths = %q, %q, %v", path, schema, err)
	}
	for _, tc := range []struct{ path, schema string }{
		{"", "S"},
		{"FRL/t", "S"},
		{"/FRL", "S"},
		{"/FRL/t/extra", "S"},
		{"/FRL//t", "S"},
		{"/FRL/t/", "S"},
		{"/FRL/..", "S"},
		{"/FRL/t", "../S"},
		{"/FRL/t", "S/other"},
		{"/FRL/t", ""},
		{"/FRL/t", "1bad"},
		{"/FRL/t", "s; DROP DATABASE /FRL/t"},
		{"/FRL/t;CREATE DATABASE /FRL/other", "S"},
		{"/FRL/t", `s"`},
		{"/FRL/t", "s--"},
		{"/FRL/t", "s s"},
		{"/FRL/%2e%2e", "S"},
		{"/__SYS", "S"},
		{"/__SYS/__SYS", "S"},
		{"/UNKNOWN/t", "S"},
	} {
		// nil DB proves all validation happens before the first DDL side effect.
		err := CreateTenant(context.Background(), nil, tc.path, tc.schema)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidPath {
			t.Errorf("CreateTenant(%q, %q) = %v, want INVALID_PATH", tc.path, tc.schema, err)
		}
	}
}

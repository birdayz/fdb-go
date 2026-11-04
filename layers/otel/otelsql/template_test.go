package otelsql

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

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

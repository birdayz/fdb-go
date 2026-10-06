package keyspace

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// The package's tests use the domains Java's tools and tests register.
func init() {
	RegisterDomainIfNotExists("FRL")
	RegisterDomainIfNotExists("TEST")
}

// ToDatabasePath is Java's toDatabasePath over KeySpaceUtils.toKeySpacePath
// against RelationalKeyspaceProvider's tree: /__SYS, or /DOMAIN/DATABASE under
// a registered domain. Each INVALID_PATH row is a shape Java's matcher refuses.
func TestToDatabasePath(t *testing.T) {
	RegisterDomainIfNotExists("FRL")
	RegisterDomainIfNotExists("TEST")
	RegisterDomainIfNotExists("FRL") // idempotent
	for _, c := range []struct {
		path string
		want DatabasePath
		err  string
	}{
		{path: "/__SYS", want: DatabasePath{System: true}},
		{path: "__SYS", want: DatabasePath{System: true}},
		{path: "//", err: "<//> is an invalid database path"}, // "/".split("/") is empty: the __SYS directory itself
		{path: "/FRL/SHOP", want: DatabasePath{Domain: "FRL", Database: "SHOP"}},
		{path: "/TEST/a-b", want: DatabasePath{Domain: "TEST", Database: "a-b"}},
		{path: "", err: "<> is an invalid database path"},
		{path: "/", err: "</> is an invalid database path"},
		{path: "/SHOP", err: "</SHOP> is an invalid database path"},
		{path: "/FRL", err: "</FRL> is an invalid database path"},
		{path: "/frl/SHOP", err: "</frl/SHOP> is an invalid database path"}, // a domain is matched exactly
		{path: "/SUPERMARIO/SHOP", err: "</SUPERMARIO/SHOP> is an invalid database path"},
		{path: "/FRL/SHOP/S", err: "</FRL/SHOP/S> is an invalid database path"},
		{path: "/FRL/SHOP/", err: "</FRL/SHOP/> is an invalid database path"},
		{path: "/FRL/SHOP/S/X", err: "</FRL/SHOP/S/X> is an invalid database path"},
		{path: "/FRL//X", err: "</FRL//X> is an invalid database path"},
		{path: "/__SYS/X", err: "</__SYS/X> is an invalid database path"},
		{path: "/FRL/IL", err: "</FRL/IL> is ambigous"}, // the interning directory and dbName both match
	} {
		got, err := ToDatabasePath(c.path)
		if c.err == "" {
			if err != nil || got != c.want {
				t.Errorf("ToDatabasePath(%q) = %+v, %v; want %+v", c.path, got, err, c.want)
			}
			continue
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidPath || apiErr.Message != c.err {
			t.Errorf("ToDatabasePath(%q) = %+v, %v; want 08F01 %q", c.path, got, err, c.err)
		}
	}
	RegisterDomainIfNotExists("SUPERMARIO")
	if got, err := ToDatabasePath("/SUPERMARIO/SHOP"); err != nil || got.Domain != "SUPERMARIO" {
		t.Errorf("a registered domain: %+v, %v", got, err)
	}
}

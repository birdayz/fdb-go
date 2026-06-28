package client

import (
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
)

func TestDBInfoEqual_BothEmpty(t *testing.T) {
	t.Parallel()
	a := &DBInfo{}
	b := &DBInfo{}
	if !dbInfoEqual(a, b) {
		t.Fatal("two empty DBInfo should be equal")
	}
}

func TestDBInfoEqual_GRVProxyLengthMismatch(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		GRVProxies: []ProxyInfo{{Address: "1.2.3.4:5000"}},
	}
	b := &DBInfo{}
	if dbInfoEqual(a, b) {
		t.Fatal("different GRVProxy count should not be equal")
	}
}

func TestDBInfoEqual_CommitProxyLengthMismatch(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		CommitProxies: []ProxyInfo{{Address: "1.2.3.4:5000"}},
	}
	b := &DBInfo{
		CommitProxies: []ProxyInfo{{Address: "1.2.3.4:5000"}, {Address: "5.6.7.8:5001"}},
	}
	if dbInfoEqual(a, b) {
		t.Fatal("different CommitProxy count should not be equal")
	}
}

func TestDBInfoEqual_GRVProxyAddressDiffers(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		GRVProxies: []ProxyInfo{{Address: "1.2.3.4:5000", Token: transport.UID{First: 1}}},
	}
	b := &DBInfo{
		GRVProxies: []ProxyInfo{{Address: "5.6.7.8:5000", Token: transport.UID{First: 1}}},
	}
	if dbInfoEqual(a, b) {
		t.Fatal("different GRVProxy addresses should not be equal")
	}
}

func TestDBInfoEqual_GRVProxyTokenDiffers(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		GRVProxies: []ProxyInfo{{Address: "1.2.3.4:5000", Token: transport.UID{First: 1, Second: 2}}},
	}
	b := &DBInfo{
		GRVProxies: []ProxyInfo{{Address: "1.2.3.4:5000", Token: transport.UID{First: 1, Second: 99}}},
	}
	if dbInfoEqual(a, b) {
		t.Fatal("different GRVProxy tokens should not be equal")
	}
}

func TestDBInfoEqual_CommitProxyAddressDiffers(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		CommitProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 10}}},
	}
	b := &DBInfo{
		CommitProxies: []ProxyInfo{{Address: "10.0.0.2:4500", Token: transport.UID{First: 10}}},
	}
	if dbInfoEqual(a, b) {
		t.Fatal("different CommitProxy addresses should not be equal")
	}
}

func TestDBInfoEqual_CommitProxyTokenDiffers(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		CommitProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 10, Second: 20}}},
	}
	b := &DBInfo{
		CommitProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 10, Second: 99}}},
	}
	if dbInfoEqual(a, b) {
		t.Fatal("different CommitProxy tokens should not be equal")
	}
}

func TestDBInfoEqual_IdenticalProxies(t *testing.T) {
	t.Parallel()
	proxy := ProxyInfo{Address: "10.0.0.1:4500", Token: transport.UID{First: 42, Second: 84}}
	a := &DBInfo{
		GRVProxies:    []ProxyInfo{proxy},
		CommitProxies: []ProxyInfo{proxy},
	}
	b := &DBInfo{
		GRVProxies:    []ProxyInfo{proxy},
		CommitProxies: []ProxyInfo{proxy},
	}
	if !dbInfoEqual(a, b) {
		t.Fatal("identical DBInfo should be equal")
	}
}

func TestDBInfoEqual_MultipleProxies(t *testing.T) {
	t.Parallel()
	a := &DBInfo{
		GRVProxies: []ProxyInfo{
			{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}},
			{Address: "10.0.0.2:4500", Token: transport.UID{First: 2}},
		},
		CommitProxies: []ProxyInfo{
			{Address: "10.0.0.3:4500", Token: transport.UID{First: 3}},
		},
	}
	b := &DBInfo{
		GRVProxies: []ProxyInfo{
			{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}},
			{Address: "10.0.0.2:4500", Token: transport.UID{First: 2}},
		},
		CommitProxies: []ProxyInfo{
			{Address: "10.0.0.3:4500", Token: transport.UID{First: 3}},
		},
	}
	if !dbInfoEqual(a, b) {
		t.Fatal("identical multi-proxy DBInfo should be equal")
	}

	// Flip second GRVProxy address — should be unequal.
	b.GRVProxies[1].Address = "10.0.0.99:4500"
	if dbInfoEqual(a, b) {
		t.Fatal("second GRVProxy address differs, should not be equal")
	}
}

//go:build !windows

package client

import "testing"

func TestUnixClusterFileDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ goos, want string }{
		{"linux", "/etc/foundationdb/fdb.cluster"},
		{"darwin", "/usr/local/etc/foundationdb/fdb.cluster"},
		{"freebsd", "/usr/local/etc/foundationdb/fdb.cluster"},
	} {
		got, err := unixClusterFilePath(tc.goos)
		if err != nil || got != tc.want {
			t.Errorf("%s default = %q, %v; want %q", tc.goos, got, err, tc.want)
		}
	}
	if _, err := unixClusterFilePath("unsupported"); err == nil {
		t.Fatal("unsupported platform silently selected a default cluster")
	}
}

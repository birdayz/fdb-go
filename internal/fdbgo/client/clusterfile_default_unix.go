//go:build !windows

package client

import (
	"fmt"
	"runtime"
)

func defaultClusterFilePath() (string, error) {
	return unixClusterFilePath(runtime.GOOS)
}

func unixClusterFilePath(goos string) (string, error) {
	switch goos {
	case "linux":
		return "/etc/foundationdb/fdb.cluster", nil
	case "darwin", "freebsd":
		return "/usr/local/etc/foundationdb/fdb.cluster", nil
	default:
		return "", fmt.Errorf("no default FDB cluster file on %s; specify a cluster file", goos)
	}
}

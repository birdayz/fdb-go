package client

import "os"

// LookupClusterFileName matches C++ ClusterConnectionFile::lookupClusterFileName:
// explicit path, present FDB_CLUSTER_FILE, local fdb.cluster, platform default.
// A configured path that cannot be read never falls back to another file.
func LookupClusterFileName(filename string) (string, error) {
	return lookupClusterFileName(filename, os.LookupEnv, func() bool {
		// C++ fileExists probes with fopen, not stat: unreadable files fall through.
		f, err := os.Open("fdb.cluster")
		if err != nil {
			return false
		}
		_ = f.Close()
		return true
	}, defaultClusterFilePath)
}

func lookupClusterFileName(filename string, lookupEnv func(string) (string, bool), localExists func() bool, defaultPath func() (string, error)) (string, error) {
	if filename != "" {
		return filename, nil
	}
	if path, ok := lookupEnv("FDB_CLUSTER_FILE"); ok {
		return path, nil
	}
	if localExists() {
		return "fdb.cluster", nil
	}
	return defaultPath()
}

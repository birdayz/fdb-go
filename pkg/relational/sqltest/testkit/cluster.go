package testkit

// ClusterFile returns the cluster file of the FoundationDB container Main
// started, or "" when Docker is unavailable.
func ClusterFile() string { return clusterFilePath }

package foundationdb

import "testing"

// `status minimal` says "unavailable" while the database is down, which a bare
// "available" substring also matches; FDB 7.3 never prints "Healthy" there, so
// a wait for it never ends early.
func TestDatabaseAvailable(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"The database is available.\n":                                       true,
		"The database is unavailable; type `status' for more information.\n": false,
		"Healthy": false,
		"":        false,
		"ERROR: Could not communicate with the cluster": false,
	} {
		if got := DatabaseAvailable(out); got != want {
			t.Errorf("DatabaseAvailable(%q) = %t, want %t", out, got, want)
		}
	}
}

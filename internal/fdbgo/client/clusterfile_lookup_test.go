package client

import (
	"errors"
	"testing"
)

func TestLookupClusterFilePrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, explicit, env, want string
		envSet, local             bool
	}{
		{"explicit", "explicit.cluster", "env.cluster", "explicit.cluster", true, true},
		{"environment", "", "missing.cluster", "missing.cluster", true, true},
		{"empty environment is present", "", "", "", true, true},
		{"working directory", "", "", "fdb.cluster", false, true},
		{"platform default", "", "", "platform.cluster", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := lookupClusterFileName(tc.explicit, func(key string) (string, bool) {
				if tc.explicit != "" {
					t.Fatal("explicit path consulted environment")
				}
				if key != "FDB_CLUSTER_FILE" {
					t.Fatalf("environment key = %q", key)
				}
				return tc.env, tc.envSet
			}, func() bool {
				if tc.explicit != "" || tc.envSet {
					t.Fatal("configured path consulted working directory")
				}
				return tc.local
			}, func() (string, error) {
				if tc.explicit != "" || tc.envSet || tc.local {
					t.Fatal("selected path consulted platform default")
				}
				return "platform.cluster", nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("lookup = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestLookupClusterFileDefaultError(t *testing.T) {
	t.Parallel()
	want := errors.New("platform path unavailable")
	_, err := lookupClusterFileName("", func(string) (string, bool) { return "", false }, func() bool { return false }, func() (string, error) { return "", want })
	if !errors.Is(err, want) {
		t.Fatalf("lookup error = %v; want %v", err, want)
	}
}

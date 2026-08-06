package cmd

import (
	"strings"
	"testing"
)

// The fleet fan-out changes what the index-build argument list MEANS: the
// index name stops being mandatory and --schema stops being allowed. Those
// rules are the whole difference between "build one index on one store" and
// "roll an index across a fleet", and they are enforced before any FDB
// connection, so they are pinned here without Docker.
//
// Each case asserts the command REFUSES. A fan-out that silently accepted
// --schema would address one store while the operator believed the fleet was
// covered — the tenant-left-behind failure this workstream exists to remove.

func TestFleetFanoutFlagContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "index build --all-schemas rejects a single-store --schema",
			args: []string{"index", "build", "--all-schemas", "--database", "/tenants", "--schema", "S1"},
			want: "drop --schema",
		},
		{
			name: "index build --all-schemas requires a database to scope the fleet",
			args: []string{"index", "build", "--all-schemas"},
			want: "needs --database",
		},
		{
			name: "index build without --all-schemas still requires the index name",
			args: []string{"index", "build"},
			want: "accepts 1 arg",
		},
		{
			name: "meta catalog repair has no single-schema form",
			args: []string{"meta", "catalog", "repair", "--database", "/tenants"},
			want: "--all-schemas",
		},
		{
			name: "meta catalog repair requires a database",
			args: []string{"meta", "catalog", "repair", "--all-schemas"},
			want: "--database is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runCmd(t, tc.args...)
			if err == nil {
				t.Fatalf("command accepted %v; it must be refused.\nout:\n%s", tc.args, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain the refusal (want it to mention %q)", err.Error(), tc.want)
			}
		})
	}
}

// TestFleetIndexBuildAcceptsAnOptionalIndexName pins that BOTH fleet shapes
// parse: a bare fan-out (build whatever each tenant owes) and a named one
// (roll exactly this index). The command is expected to fail LATER, on the
// cluster connection, not on argument validation — so this asserts the
// failure is NOT an argument-shape complaint.
func TestFleetIndexBuildAcceptsAnOptionalIndexName(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"index", "build", "--all-schemas", "--database", "/tenants", "--yes", "--cluster-file", "/nonexistent-cluster-file"},
		{"index", "build", "IDX", "--all-schemas", "--database", "/tenants", "--yes", "--cluster-file", "/nonexistent-cluster-file"},
	} {
		_, err := runCmd(t, args...)
		if err == nil {
			continue // reached the cluster somehow; argument shape was fine either way
		}
		for _, shapeComplaint := range []string{"accepts 1 arg", "drop --schema", "needs --database"} {
			if strings.Contains(err.Error(), shapeComplaint) {
				t.Fatalf("%v was rejected on argument shape (%q); both fleet shapes must parse",
					args, err.Error())
			}
		}
	}
}

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
			args: []string{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--schema", "S1"},
			want: "drop --schema",
		},
		{
			name: "fleet build rejects descriptor metadata",
			args: []string{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--descriptor-set", "schema.binpb"},
			want: "cannot be combined with --descriptor-set",
		},
		{
			name: "fleet build rejects records-file selection",
			args: []string{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--records-file", "records.proto"},
			want: "cannot be combined with --records-file",
		},
		{
			name: "fleet build rejects explicit metadata version zero",
			args: []string{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--meta-version", "0"},
			want: "cannot be combined with --meta-version",
		},
		{
			name: "fleet build rejects metadata artifacts",
			args: []string{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--meta-file", "metadata.pb"},
			want: "cannot be combined with --meta-file",
		},
		{
			name: "fleet build rejects a native keyspace",
			args: []string{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--keyspace-tuple", `["native"]`},
			want: "cannot be combined with --keyspace-tuple",
		},
		{
			name: "index build --all-schemas requires a database to scope the fleet",
			args: []string{"rl", "index", "build", "--all-schemas"},
			want: "needs --database",
		},
		{
			name: "index build without --all-schemas still requires the index name",
			args: []string{"rl", "index", "build"},
			want: "accepts 1 arg",
		},
		{
			name: "meta catalog repair has no single-schema form",
			args: []string{"rl", "meta", "catalog", "repair", "--database", "/FRL/tenants"},
			want: "--all-schemas",
		},
		{
			name: "meta catalog repair requires a database",
			args: []string{"rl", "meta", "catalog", "repair", "--all-schemas"},
			// The flag name, not the surrounding prose — same as the cases
			// above.
			want: "--database",
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
		{"rl", "index", "build", "--all-schemas", "--database", "/FRL/tenants", "--yes", "--cluster-file", "/nonexistent-cluster-file"},
		{"rl", "index", "build", "IDX", "--all-schemas", "--database", "/FRL/tenants", "--yes", "--cluster-file", "/nonexistent-cluster-file"},
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

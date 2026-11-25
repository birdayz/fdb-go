package fdb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/internal/fdbgo/client"
)

func TestMetricsSnapshotMatchesClient(t *testing.T) {
	t.Parallel()

	assertStructFieldsMatch(t, reflect.TypeOf(LatencyStats{}), reflect.TypeOf(client.LatencyStats{}))
	assertStructFieldsMatch(t, reflect.TypeOf(MetricsSnapshot{}), reflect.TypeOf(client.ClientMetricsSnapshot{}))

	// Zero Database returns zero MetricsSnapshot without panicking.
	if got := (Database{}).Metrics(); got != (MetricsSnapshot{}) {
		t.Fatalf("zero Database.Metrics() = %+v, want zero", got)
	}

	src := client.ClientMetricsSnapshot{
		TransactionsCommitStarted:                 1,
		TransactionsCommitCompleted:               2,
		TransactionsNotCommitted:                  3,
		TransactionsMaybeCommitted:                4,
		TransactionsResourceConstrained:           5,
		TransactionsProcessBehind:                 6,
		TransactionsThrottled:                     7,
		TransactionsTooOld:                        8,
		TransactionsFutureVersions:                9,
		TransactionReadVersionsCompleted:          10,
		TransactionBatchReadVersionsCompleted:     11,
		TransactionDefaultReadVersionsCompleted:   12,
		TransactionImmediateReadVersionsCompleted: 13,
		GRVCacheHits:                              14,
		GRVInBandMaybeDelivered:                   15,
		TransactionRetries:                        16,
		TransactionGetRangeRequests:               17,
		RecoveredPanics:                           18,
		RecoveredPanicsConsecutiveMax:             19,
		ClientConnectionFailures:                  20,
		CoordinatorChanges:                        21,
		ReadLatency:                               client.LatencyStats{Count: 22, Sum: 1.1, Mean: 0.05, Median: 0.04, P90: 0.09, P99: 0.2, Min: 0.01, Max: 0.3},
		CommitLatency:                             client.LatencyStats{Count: 23, Sum: 2.2, Mean: 0.1, Median: 0.08, P90: 0.18, P99: 0.4, Min: 0.02, Max: 0.5},
		GRVLatency:                                client.LatencyStats{Count: 24, Sum: 3.3, Mean: 0.15, Median: 0.12, P90: 0.27, P99: 0.6, Min: 0.03, Max: 0.7},
		TransactionLatency:                        client.LatencyStats{Count: 25, Sum: 4.4, Mean: 0.2, Median: 0.16, P90: 0.36, P99: 0.8, Min: 0.04, Max: 0.9},
	}
	got := fromClientMetricsSnapshot(src)
	assertAllFieldsEqual(t, reflect.ValueOf(got), reflect.ValueOf(src))
}

func assertStructFieldsMatch(t *testing.T, pub, cli reflect.Type) {
	t.Helper()
	if pub.NumField() != cli.NumField() {
		t.Fatalf("%s has %d fields, client %s has %d", pub.Name(), pub.NumField(), cli.Name(), cli.NumField())
	}
	for i := range pub.NumField() {
		pf, cf := pub.Field(i), cli.Field(i)
		if pf.Name != cf.Name || pf.Type.Kind() != cf.Type.Kind() {
			t.Fatalf("field %d: pub %s (%v) != client %s (%v)", i, pf.Name, pf.Type, cf.Name, cf.Type)
		}
		if pf.Type.Kind() == reflect.Struct {
			assertStructFieldsMatch(t, pf.Type, cf.Type)
		}
	}
}

func assertAllFieldsEqual(t *testing.T, pub, cli reflect.Value) {
	t.Helper()
	for i := range pub.NumField() {
		pf, cf := pub.Field(i), cli.Field(i)
		name := pub.Type().Field(i).Name
		if pf.Kind() == reflect.Struct {
			assertAllFieldsEqual(t, pf, cf)
			continue
		}
		if !reflect.DeepEqual(pf.Interface(), cf.Interface()) {
			t.Errorf("field %s: got %v, want %v", name, pf.Interface(), cf.Interface())
		}
	}
}

func TestParseClusterFileAndString(t *testing.T) {
	t.Parallel()

	const connStr = "prod:abc123@10.0.0.2:4500:tls,10.0.0.1:4500:tls"
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("ParseClusterString: %v", err)
	}
	wantClient, err := client.ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("client.ParseClusterString: %v", err)
	}
	if cf.Description != wantClient.Description || cf.ID != wantClient.ID ||
		cf.UseTLS != wantClient.UseTLS || !slices.Equal(cf.Coordinators, wantClient.Coordinators) ||
		cf.String() != wantClient.String() {
		t.Fatalf("ParseClusterString mismatch: got %+v (%q), want %+v (%q)", cf, cf.String(), wantClient, wantClient.String())
	}

	path := filepath.Join(t.TempDir(), "fdb.cluster")
	content := "# comment\n\n" + connStr + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := ParseClusterFile(path)
	if err != nil {
		t.Fatalf("ParseClusterFile: %v", err)
	}
	if fromFile.String() != cf.String() || !fromFile.UseTLS || len(fromFile.Coordinators) != 2 {
		t.Fatalf("ParseClusterFile = %+v (%q), want %q", fromFile, fromFile.String(), cf.String())
	}

	// Round-trip conversion clones Coordinators so caller mutations do not alias.
	cliCF := toClientClusterFile(fromFile)
	cliCF.Coordinators[0] = "mutated:1"
	if fromFile.Coordinators[0] == "mutated:1" {
		t.Fatal("toClientClusterFile aliased Coordinators slice")
	}
	if fromClientClusterFile(nil) != nil || toClientClusterFile(nil) != nil || (*ClusterFile)(nil).String() != "" {
		t.Fatal("nil ClusterFile handling failed")
	}

	if _, err := ParseClusterString("missing_at_sign"); err == nil {
		t.Fatal("ParseClusterString accepted invalid string")
	}
	if _, err := ParseClusterFile(filepath.Join(t.TempDir(), "missing.cluster")); err == nil {
		t.Fatal("ParseClusterFile accepted missing file")
	}
}

func TestMappedKeyValueLayoutMatchesClient(t *testing.T) {
	t.Parallel()

	assertLayout := func(pub, cli reflect.Type) {
		t.Helper()
		if pub.Size() != cli.Size() || pub.Align() != cli.Align() || pub.NumField() != cli.NumField() {
			t.Fatalf("%s layout %d/%d/%d differs from client %s %d/%d/%d",
				pub.Name(), pub.Size(), pub.Align(), pub.NumField(), cli.Name(), cli.Size(), cli.Align(), cli.NumField())
		}
		for i := range pub.NumField() {
			p, c := pub.Field(i), cli.Field(i)
			if p.Name != c.Name || p.Offset != c.Offset || p.Type.Size() != c.Type.Size() || p.Type.Kind() != c.Type.Kind() {
				t.Fatalf("%s field %d: %s %v @%d differs from client %s %v @%d",
					pub.Name(), i, p.Name, p.Type, p.Offset, c.Name, c.Type, c.Offset)
			}
		}
	}

	assertLayout(reflect.TypeOf(MappedKeySelector{}), reflect.TypeOf(client.KeySelector{}))
	assertLayout(reflect.TypeOf(MappedGetValue{}), reflect.TypeOf(client.MappedGetValue{}))
	assertLayout(reflect.TypeOf(MappedGetRange{}), reflect.TypeOf(client.MappedGetRange{}))
	assertLayout(reflect.TypeOf(MappedKeyValue{}), reflect.TypeOf(client.MappedKeyValue{}))

	for _, tc := range []struct {
		pub MappedResultKind
		cli client.MappedResultKind
	}{
		{MappedResultNone, client.MappedResultNone},
		{MappedResultGetValue, client.MappedResultGetValue},
		{MappedResultGetRange, client.MappedResultGetRange},
		{MappedResultKind(99), client.MappedResultKind(99)},
	} {
		if uint8(tc.pub) != uint8(tc.cli) || tc.pub.String() != tc.cli.String() {
			t.Fatalf("MappedResultKind(%d): got %q, want %q", tc.pub, tc.pub.String(), tc.cli.String())
		}
	}
}

func TestOptionAndDialFuncAndErrors(t *testing.T) {
	t.Parallel()

	var dial DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("dial %s %s", network, addr)
	}
	clientOpts := toClientOptions([]Option{
		nil,
		WithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}),
		WithDialFunc(dial),
		WithDialFunc(nil),
		WithCoalesceDelay(time.Microsecond, 2*time.Microsecond),
		WithAdaptiveLinger(3 * time.Microsecond),
		WithRangeByteCeiling(1024),
		WithTracingSampleRate(0.5),
		WithTracer(oteltrace.Tracer(nil)),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithLogger(nil),
	}, 730)
	// 10 non-nil options + WithAPIVersion(730).
	if len(clientOpts) != 11 {
		t.Fatalf("toClientOptions returned %d options, want 11", len(clientOpts))
	}

	// RangeMaterializationLimitError is converted from client.RangeMaterializationLimitError
	// without leaking the client error type, while preserving non-client wrapped causes.
	directConverted := convertError(&client.RangeMaterializationLimitError{LimitBytes: 100, ReachedBytes: 250})
	var pubLimit *RangeMaterializationLimitError
	if !errors.As(directConverted, &pubLimit) || pubLimit.LimitBytes != 100 || pubLimit.ReachedBytes != 250 || pubLimit.Cause != nil {
		t.Fatalf("convertError(direct) = %T (%v), want *RangeMaterializationLimitError{100, 250, nil}", directConverted, directConverted)
	}

	cliErr := fmt.Errorf("wrapped: %w", &client.RangeMaterializationLimitError{LimitBytes: 100, ReachedBytes: 250})
	converted := convertError(cliErr)
	pubLimit = nil
	if !errors.As(converted, &pubLimit) || pubLimit.LimitBytes != 100 || pubLimit.ReachedBytes != 250 {
		t.Fatalf("convertError = %T (%v), want *RangeMaterializationLimitError{100, 250}", converted, converted)
	}
	var cliLimit *client.RangeMaterializationLimitError
	if errors.As(converted, &cliLimit) {
		t.Fatalf("convertError leaked *client.RangeMaterializationLimitError: %v", converted)
	}

	sentinel := errors.New("range aborted")
	joinedErr := fmt.Errorf("outer: %w", errors.Join(fmt.Errorf("ctx: %w", sentinel), &client.RangeMaterializationLimitError{LimitBytes: 64, ReachedBytes: 128}))
	joinedConverted := convertError(joinedErr)
	pubLimit = nil
	if !errors.As(joinedConverted, &pubLimit) || pubLimit.LimitBytes != 64 || pubLimit.ReachedBytes != 128 {
		t.Fatalf("convertError(joined) = %T (%v), want *RangeMaterializationLimitError{64, 128}", joinedConverted, joinedConverted)
	}
	if !errors.Is(joinedConverted, sentinel) {
		t.Fatalf("convertError(joined) dropped wrapped sentinel cause: %v", joinedConverted)
	}
	if errors.As(joinedConverted, &cliLimit) {
		t.Fatalf("convertError(joined) leaked *client.RangeMaterializationLimitError: %v", joinedConverted)
	}
}

func TestPublicAPINoClientLeakage(t *testing.T) {
	t.Parallel()

	forbiddenPrefixes := []string{
		"fdb.dev/internal/fdbgo/client",
		"fdb.dev/internal/fdbgo/transport",
		"fdb.dev/internal/fdbwire",
	}

	visited := make(map[reflect.Type]bool)
	var checkType func(path string, typ reflect.Type)
	checkType = func(path string, typ reflect.Type) {
		if typ == nil || visited[typ] {
			return
		}
		visited[typ] = true

		if pkg := typ.PkgPath(); pkg != "" {
			for _, forbidden := range forbiddenPrefixes {
				if pkg == forbidden || strings.HasPrefix(pkg, forbidden+"/") {
					t.Errorf("%s leaks internal type %s (pkg %s)", path, typ, pkg)
					return
				}
			}
		}

		for i := range typ.NumMethod() {
			m := typ.Method(i)
			if !m.IsExported() {
				continue
			}
			checkType(path+"."+m.Name, m.Type)
		}

		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			checkType(path, typ.Elem())
		case reflect.Map:
			checkType(path+"[key]", typ.Key())
			checkType(path+"[val]", typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				if !f.IsExported() {
					continue
				}
				checkType(path+"."+f.Name, f.Type)
			}
		case reflect.Func:
			for i := range typ.NumIn() {
				checkType(fmt.Sprintf("%s(in[%d])", path, i), typ.In(i))
			}
			for i := range typ.NumOut() {
				checkType(fmt.Sprintf("%s(out[%d])", path, i), typ.Out(i))
			}
		case reflect.Interface:
			for i := range typ.NumMethod() {
				m := typ.Method(i)
				if !m.IsExported() {
					continue
				}
				checkType(path+"."+m.Name, m.Type)
			}
		}
	}

	// Inspect all decoupled public types and functions (excluding explicit
	// client-bridging constructors WrapDatabase / WrapDatabaseContext / WrapTransaction).
	publicSymbols := map[string]any{
		"Database":                       Database{},
		"Transaction":                    Transaction{},
		"Snapshot":                       Snapshot{},
		"Tenant":                         Tenant{},
		"DatabaseOptions":                DatabaseOptions{},
		"ClusterFile":                    ClusterFile{},
		"MetricsSnapshot":                MetricsSnapshot{},
		"LatencyStats":                   LatencyStats{},
		"MappedKeyValue":                 MappedKeyValue{},
		"MappedGetValue":                 MappedGetValue{},
		"MappedGetRange":                 MappedGetRange{},
		"MappedKeySelector":              MappedKeySelector{},
		"MappedResultKind":               MappedResultKind(0),
		"RangeMaterializationLimitError": RangeMaterializationLimitError{},
		"Option":                         Option(nil),
		"DialFunc":                       DialFunc(nil),
		"OpenDatabase":                   OpenDatabase,
		"OpenWithConnectionString":       OpenWithConnectionString,
		"OpenDatabaseFromConfig":         OpenDatabaseFromConfig,
		"ParseClusterFile":               ParseClusterFile,
		"ParseClusterString":             ParseClusterString,
		"WithTLSConfig":                  WithTLSConfig,
		"WithDialFunc":                   WithDialFunc,
		"WithCoalesceDelay":              WithCoalesceDelay,
		"WithAdaptiveLinger":             WithAdaptiveLinger,
		"WithRangeByteCeiling":           WithRangeByteCeiling,
		"WithTracingSampleRate":          WithTracingSampleRate,
		"WithTracer":                     WithTracer,
		"WithLogger":                     WithLogger,
	}
	for name, sym := range publicSymbols {
		checkType(name, reflect.TypeOf(sym))
	}
	interfaceTypes := map[string]reflect.Type{
		"BackendDatabase":       reflect.TypeOf((*BackendDatabase)(nil)).Elem(),
		"ReadTransaction":       reflect.TypeOf((*ReadTransaction)(nil)).Elem(),
		"WritableTransaction":   reflect.TypeOf((*WritableTransaction)(nil)).Elem(),
		"TransactionOptions":    reflect.TypeOf((*TransactionOptions)(nil)).Elem(),
		"RangeResult":           reflect.TypeOf((*RangeResult)(nil)).Elem(),
		"RangeIterator":         reflect.TypeOf((*RangeIterator)(nil)).Elem(),
		"MappedRangeResult":     reflect.TypeOf((*MappedRangeResult)(nil)).Elem(),
		"MappedRangeIterator":   reflect.TypeOf((*MappedRangeIterator)(nil)).Elem(),
		"MappedRangeReader":     reflect.TypeOf((*MappedRangeReader)(nil)).Elem(),
		"MappedRangeSkipReader": reflect.TypeOf((*MappedRangeSkipReader)(nil)).Elem(),
	}
	for name, typ := range interfaceTypes {
		checkType(name, typ)
	}
}

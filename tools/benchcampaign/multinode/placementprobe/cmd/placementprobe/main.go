// Command placementprobe is the multi-node campaign's placement probe (H5) and
// ballast/control writer (H6). See package placementprobe and the campaign
// protocol's placement section.
//
//	placementprobe probe                --cluster-file F --store S --fixture-receipt R --output O [--allow-dd-on]
//	placementprobe ballast-write        --cluster-file F --store S --fixture-receipt R --ballast-mib N --output O
//	placementprobe ballast-clear        --cluster-file F --store S --fixture-receipt R --output O
//	placementprobe ballast-verify-empty --cluster-file F --store S --fixture-receipt R --output O
//	placementprobe control-write        --cluster-file F --store benchfetch/control-{small,split} [--rows N] --fixture-receipt-out R --output O
//	placementprobe control-mutate       --cluster-file F --store benchfetch/control-split --defect {delete-chunk,stray-suffix,version-key} --output O
//	placementprobe control-clear        --cluster-file F --store benchfetch/control-{small,split} --output O
//
// Every output is created exclusively, outside any Git work tree. Exit status:
// 0 success, 1 the operation ran but failed (receipt written when possible),
// 2 refused or unusable input (nothing touched).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/tools/benchcampaign/multinode/placementprobe"
)

const apiVersion = 730

var commands = []string{
	"probe", "ballast-write", "ballast-clear", "ballast-verify-empty", "control-write", "control-mutate", "control-clear",
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	clusterFile, store, fixture, output, fixtureOut, defect string
	ballastMiB, timeoutMillis                               int64
	rows                                                    int
	allowDDOn                                               bool
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "usage: placementprobe {%s} [flags]; stores: %s\n",
		strings.Join(commands, "|"), strings.Join(placementprobe.Stores(), ", "))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd := args[0]
	known := false
	for _, c := range commands {
		known = known || c == cmd
	}
	if !known {
		usage(stderr)
		return 2
	}
	var o options
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.clusterFile, "cluster-file", "", "FoundationDB cluster file (required)")
	fs.StringVar(&o.store, "store", "", "owned store name (required)")
	fs.StringVar(&o.fixture, "fixture-receipt", "", "benchmark fixture receipt: JSON object or log with FDB_BENCH_AUDIT lines")
	fs.StringVar(&o.output, "output", "", "new receipt/probe file outside any Git work tree (required)")
	fs.StringVar(&o.fixtureOut, "fixture-receipt-out", "", "control-write: new fixture receipt file for the probe")
	fs.StringVar(&o.defect, "defect", "", "control-mutate: delete-chunk, stray-suffix or version-key")
	fs.Int64Var(&o.ballastMiB, "ballast-mib", 0, "ballast-write: ballast size in MiB")
	fs.Int64Var(&o.timeoutMillis, "transaction-timeout-ms", 30000, "per-transaction timeout")
	fs.IntVar(&o.rows, "rows", 1000, "control-write: fixture rows")
	fs.BoolVar(&o.allowDDOn, "allow-dd-on", false, "probe: diagnostic probe while data distribution is on")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if err := execute(cmd, o, stdout); err != nil {
		fmt.Fprintf(stderr, "placementprobe %s: %v\n", cmd, err)
		var failed operationFailed
		if errors.As(err, &failed) {
			return 1
		}
		return 2
	}
	return 0
}

// operationFailed marks an error after the cluster was touched.
type operationFailed struct{ err error }

func (e operationFailed) Error() string { return e.err.Error() }
func (e operationFailed) Unwrap() error { return e.err }

func checkOutput(path string) error {
	if path == "" {
		return errors.New("--output is required")
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s exists; outputs are never overwritten", path)
	}
	inside, err := placementprobe.InsideGitWorktree(path)
	if err != nil {
		return err
	}
	if inside {
		return fmt.Errorf("%s is inside a Git work tree", path)
	}
	return nil
}

// resolve returns the store prefix and, when a receipt was given, its SHA-256.
func resolve(o options, control bool) ([]byte, string, error) {
	if control {
		if o.fixture != "" {
			return nil, "", errors.New("control commands take no --fixture-receipt")
		}
		p, err := placementprobe.ControlPrefix(o.store)
		return p, "", err
	}
	if o.fixture == "" {
		return nil, "", errors.New("--fixture-receipt is required: the prefix comes from the owned fixture receipt")
	}
	data, err := os.ReadFile(o.fixture)
	if err != nil {
		return nil, "", err
	}
	r, err := placementprobe.ParseFixtureReceipt(data, o.store)
	if err != nil {
		return nil, "", err
	}
	p, err := placementprobe.ResolvePrefix(o.store, r)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return p, hex.EncodeToString(sum[:]), nil
}

func execute(cmd string, o options, stdout io.Writer) error {
	if o.clusterFile == "" || o.store == "" {
		return errors.New("--cluster-file and --store are required")
	}
	if err := checkOutput(o.output); err != nil {
		return err
	}
	control := strings.HasPrefix(cmd, "control-")
	if !control && strings.HasPrefix(o.store, "benchfetch/control-") && cmd != "probe" {
		return errors.New("ballast commands refuse the scratch control stores")
	}
	var prefix []byte
	var fixtureSHA string
	var err error
	if cmd == "probe" && strings.HasPrefix(o.store, "benchfetch/control-") {
		prefix, fixtureSHA, err = resolve(o, false) // the control-write fixture receipt
	} else {
		prefix, fixtureSHA, err = resolve(o, control)
	}
	if err != nil {
		return err
	}
	switch cmd {
	case "ballast-write":
		if o.ballastMiB <= 0 || o.ballastMiB > 4096 {
			return errors.New("--ballast-mib must be in 1..4096")
		}
	case "control-write":
		if err := checkOutput(o.fixtureOut); err != nil {
			return fmt.Errorf("--fixture-receipt-out: %w", err)
		}
	case "control-mutate":
		if o.defect == "" {
			return errors.New("--defect is required")
		}
	}
	if err := fdb.APIVersion(apiVersion); err != nil {
		return err
	}
	db, err := fdb.OpenDatabase(o.clusterFile)
	if err != nil {
		return err
	}
	defer db.Close()
	c := placementprobe.FDBCluster{DB: db, TimeoutMillis: o.timeoutMillis, MaxAttempts: 10}
	var out any
	var opErr error
	switch cmd {
	case "probe":
		out, opErr = placementprobe.Probe(c, placementprobe.ProbeInput{
			Store: o.store, Prefix: prefix, FixtureSHA256: fixtureSHA, AllowDDOn: o.allowDDOn,
		})
	case "ballast-write":
		out, opErr = placementprobe.WriteBallast(c, o.store, prefix, o.ballastMiB<<20)
	case "ballast-clear":
		out, opErr = placementprobe.ClearBallast(c, o.store, prefix)
	case "ballast-verify-empty":
		out, opErr = placementprobe.VerifyBallastEmpty(c, o.store, prefix)
	case "control-write":
		r, fx, err := placementprobe.WriteControlFixture(c, o.store, prefix, o.rows)
		if err == nil {
			err = placementprobe.WriteJSONExclusive(o.fixtureOut, fx)
		}
		out, opErr = r, err
	case "control-mutate":
		out, opErr = placementprobe.MutateControl(c, o.store, prefix, o.defect)
	case "control-clear":
		out, opErr = placementprobe.ClearControl(c, o.store, prefix)
	}
	if !isNil(out) {
		if err := placementprobe.WriteJSONExclusive(o.output, out); err != nil {
			return operationFailed{errors.Join(opErr, err)}
		}
		fmt.Fprintln(stdout, o.output)
	}
	if opErr != nil {
		return operationFailed{opErr}
	}
	return nil
}

// isNil reports a nil interface or a typed nil pointer result.
func isNil(v any) bool {
	switch p := v.(type) {
	case nil:
		return true
	case *placementprobe.ProbeDoc:
		return p == nil
	case *placementprobe.BallastReceipt:
		return p == nil
	}
	return false
}

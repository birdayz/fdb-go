// Command placementprobe is the multi-node campaign's placement probe (H5) and
// ballast/control writer (H6). See package placementprobe and the campaign
// protocol's placement section. Build placementprobe_cgo (libfdb_c) for
// placement receipts; placementprobe_go has location-cache addresses and its
// probes require --diagnostic.
//
//	placementprobe probe                --cluster-file F --store S --fixture-receipt R --output O [--diagnostic]
//	placementprobe ballast-write        --cluster-file F --store S --fixture-receipt R --ballast-mib N --output O
//	placementprobe ballast-clear        --cluster-file F --store S --fixture-receipt R --output O
//	placementprobe ballast-verify-empty --cluster-file F --store S --fixture-receipt R --output O
//	placementprobe control-write        --cluster-file F --store benchfetch/control-{small,split} [--rows N] --fixture-receipt-out R --output O
//	placementprobe control-mutate       --cluster-file F --store benchfetch/control-split --defect {delete-chunk,stray-suffix,version-key} --output O
//	placementprobe control-clear        --cluster-file F --store benchfetch/control-{small,split} --output O
//
// Every document records the cluster identity (cluster file SHA-256 and
// description:id). Every output is created exclusively, outside any Git work
// tree. Exit status: 0 success, 1 the operation ran but failed (receipt
// written when possible), 2 refused or unusable input (nothing touched).
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
	diagnostic                                              bool
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
	fs.BoolVar(&o.diagnostic, "diagnostic", false, "probe: allow location-cache addresses and DD on (never sealable)")
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

// openStore returns the owned store and, when a fixture receipt was given,
// the receipt file's SHA-256.
func openStore(cmd string, o options) (placementprobe.OwnedStore, string, error) {
	control := strings.HasPrefix(o.store, "benchfetch/control-")
	if strings.HasPrefix(cmd, "control-") {
		if o.fixture != "" {
			return placementprobe.OwnedStore{}, "", errors.New("control commands take no --fixture-receipt")
		}
		s, err := placementprobe.OpenControlStore(o.store)
		return s, "", err
	}
	if control && cmd != "probe" {
		return placementprobe.OwnedStore{}, "", errors.New("ballast commands refuse the scratch control stores")
	}
	if o.fixture == "" {
		return placementprobe.OwnedStore{}, "", errors.New("--fixture-receipt is required: the prefix comes from the owned fixture receipt")
	}
	data, err := os.ReadFile(o.fixture)
	if err != nil {
		return placementprobe.OwnedStore{}, "", err
	}
	r, err := placementprobe.ParseFixtureReceipt(data, o.store)
	if err != nil {
		return placementprobe.OwnedStore{}, "", err
	}
	s, err := placementprobe.OpenStore(o.store, r)
	if err != nil {
		return placementprobe.OwnedStore{}, "", err
	}
	sum := sha256.Sum256(data)
	return s, hex.EncodeToString(sum[:]), nil
}

func execute(cmd string, o options, stdout io.Writer) error {
	if o.clusterFile == "" || o.store == "" {
		return errors.New("--cluster-file and --store are required")
	}
	if err := checkOutput(o.output); err != nil {
		return err
	}
	clusterBytes, err := os.ReadFile(o.clusterFile)
	if err != nil {
		return err
	}
	identity, err := placementprobe.ParseClusterIdentity(clusterBytes)
	if err != nil {
		return err
	}
	store, fixtureSHA, err := openStore(cmd, o)
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
	c, closeDB, err := openCluster(o.clusterFile, o.timeoutMillis)
	if err != nil {
		return err
	}
	defer closeDB()
	var out any
	var receipt *placementprobe.BallastReceipt
	var opErr error
	switch cmd {
	case "probe":
		var doc *placementprobe.ProbeDoc
		doc, opErr = placementprobe.Probe(c, placementprobe.ProbeInput{
			Store: store, FixtureSHA256: fixtureSHA, Cluster: identity, Diagnostic: o.diagnostic,
		})
		if doc != nil {
			out = doc
		}
	case "ballast-write":
		receipt, opErr = placementprobe.WriteBallast(c, store, o.ballastMiB<<20)
	case "ballast-clear":
		receipt, opErr = placementprobe.ClearBallast(c, store)
	case "ballast-verify-empty":
		receipt, opErr = placementprobe.VerifyBallastEmpty(c, store)
	case "control-write":
		var fx placementprobe.FixtureReceipt
		receipt, fx, opErr = placementprobe.WriteControlFixture(c, store, o.rows)
		if opErr == nil {
			opErr = placementprobe.WriteJSONExclusive(o.fixtureOut, fx)
		}
	case "control-mutate":
		receipt, opErr = placementprobe.MutateControl(c, store, o.defect)
	case "control-clear":
		receipt, opErr = placementprobe.ClearControl(c, store)
	}
	if receipt != nil {
		receipt.Cluster = identity
		out = receipt
	}
	if out != nil {
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

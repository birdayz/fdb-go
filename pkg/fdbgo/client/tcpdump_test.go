package client

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestCaptureCBindingTraffic starts a tcpdump inside the container,
// does a C binding transaction, then reads the captured packets to
// see what bytes the C binding sends for OpenDatabaseCoordRequest.
func TestCaptureCBindingTraffic(t *testing.T) {
	t.Skip("manual test — run with -test.run=TestCaptureCBindingTraffic")
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer container.Terminate(ctx)

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	t.Logf("connection string: %s", connStr)

	// Start tcpdump inside the container to capture FDB traffic
	container.Exec(ctx, []string{
		"bash", "-c", "tcpdump -i any -w /tmp/fdb.pcap port 4500 &",
	})
	time.Sleep(1 * time.Second)

	// Configure cluster and do a C binding transaction
	container.Exec(ctx, []string{
		"fdbcli", "--exec", "configure new single ssd",
	})
	time.Sleep(2 * time.Second)

	// C binding transaction
	fdb.MustAPIVersion(720)
	tmpFile, _ := os.CreateTemp("", "fdb-*.cluster")
	tmpFile.WriteString(connStr)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		t.Logf("C binding open: %v", err)
	} else {
		_, err = db.Transact(func(tx fdb.Transaction) (any, error) {
			tx.Set(fdb.Key("test"), []byte("hello"))
			return nil, nil
		})
		t.Logf("C binding transact: %v", err)
	}

	// Wait for traffic to be captured
	time.Sleep(2 * time.Second)

	// Stop tcpdump and read the capture
	container.Exec(ctx, []string{"bash", "-c", "kill %1 2>/dev/null; sync"})
	time.Sleep(1 * time.Second)

	// Read the pcap file
	_, reader, err := container.Exec(ctx, []string{"cat", "/tmp/fdb.pcap"})
	if err != nil {
		t.Fatalf("read pcap: %v", err)
	}
	pcapData, _ := io.ReadAll(reader)
	t.Logf("pcap size: %d bytes", len(pcapData))

	// Write pcap to temp file for analysis
	pcapFile := "/tmp/fdb_traffic.pcap"
	os.WriteFile(pcapFile, pcapData, 0o644)
	t.Logf("wrote pcap to %s", pcapFile)

	// Also try reading raw hex of the first few packets
	_, hexReader, _ := container.Exec(ctx, []string{
		"bash", "-c", "tcpdump -r /tmp/fdb.pcap -xx -c 10 2>/dev/null || echo 'tcpdump not available for reading'",
	})
	if hexReader != nil {
		hexData, _ := io.ReadAll(hexReader)
		fmt.Println(string(hexData))
	}

	_ = hex.EncodeToString
}

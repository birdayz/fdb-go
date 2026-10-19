// Command wiregolden prints the normalized request transcript of a capture
// (see internal/wiregolden). It fails on an incomplete capture or one without
// a scenario.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"fdb.dev/tools/clientbench/internal/wiregolden"
)

func main() {
	pcap := flag.String("pcap", "", "capture file")
	ports := flag.String("server-ports", "", "comma-separated fdbserver ports")
	prefix := flag.String("prefix", "\x02wg/d/", "scenario key prefix, written <p>")
	flag.Parse()
	if err := run(*pcap, *ports, *prefix); err != nil {
		fmt.Fprintln(os.Stderr, "wiregolden:", err)
		os.Exit(1)
	}
}

func run(pcap, ports, prefix string) error {
	if pcap == "" || ports == "" {
		return fmt.Errorf("-pcap and -server-ports are required")
	}
	var serverPorts []uint16
	for _, p := range strings.Split(ports, ",") {
		n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 16)
		if err != nil {
			return fmt.Errorf("server port %q: %w", p, err)
		}
		serverPorts = append(serverPorts, uint16(n))
	}
	f, err := os.Open(pcap)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := wiregolden.Decode(bufio.NewReaderSize(f, 1<<20), serverPorts)
	if err != nil {
		return err
	}
	if !c.Complete {
		return fmt.Errorf("incomplete capture (%d undecodable requests)", c.Undecodable)
	}
	t := wiregolden.Transcript(c.Requests, prefix)
	if !strings.HasPrefix(t, "== ") {
		return fmt.Errorf("no scenario marker in the capture")
	}
	if len(c.Other) > 0 {
		fmt.Fprintf(os.Stderr, "wiregolden: client frames not in the transcript, by file identifier: %v\n", c.Other)
	}
	_, err = os.Stdout.WriteString(t)
	return err
}

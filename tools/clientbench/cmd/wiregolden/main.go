// Command wiregolden prints the normalized request transcript of a capture
// (see internal/wiregolden). It fails on an incomplete capture, or unless the
// capture holds exactly the scenarios listed in -expect (one name per line,
// wgscen -list), closed by the ~end marker.
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
	expect := flag.String("expect", "", "file listing the scenario names in order")
	flag.Parse()
	if err := run(*pcap, *ports, *prefix, *expect); err != nil {
		fmt.Fprintln(os.Stderr, "wiregolden:", err)
		os.Exit(1)
	}
}

func run(pcap, ports, prefix, expect string) error {
	if pcap == "" || ports == "" || expect == "" {
		return fmt.Errorf("-pcap, -server-ports and -expect are required")
	}
	list, err := os.ReadFile(expect)
	if err != nil {
		return err
	}
	want := strings.Fields(string(list))
	if len(want) == 0 {
		return fmt.Errorf("%s lists no scenario", expect)
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
	t, err := wiregolden.Transcript(c.Requests, prefix, want)
	if err != nil {
		return err
	}
	if len(c.Other) > 0 {
		fmt.Fprintf(os.Stderr, "wiregolden: client frames not in the transcript, by file identifier: %v\n", c.Other)
	}
	_, err = os.Stdout.WriteString(t)
	return err
}

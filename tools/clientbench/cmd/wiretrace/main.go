// Command wiretrace summarizes the FDB GRV and commit traffic in a loopback
// packet capture: requests per server batch, client segments per request, and
// arrival spread within batches. Capture with tcpdump -s 0 on lo, starting
// before the client connects (scripts/wirecapture.sh).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"fdb.dev/tools/clientbench/internal/wiretrace"
)

func main() {
	pcap := flag.String("pcap", "", "classic pcap capture (tcpdump -w)")
	ports := flag.String("server-ports", "", "comma-separated fdbserver ports")
	events := flag.String("events", "", "write one CSV line per request to this file")
	round := flag.Int("round", 0, "closed-loop round size (workers): report whole and split commit batches")
	allowIncomplete := flag.Bool("allow-incomplete", false, "summarize a capture with stream gaps or cut-off frames instead of failing")
	flag.Parse()
	if err := run(*pcap, *ports, *events, *round, *allowIncomplete); err != nil {
		fmt.Fprintln(os.Stderr, "wiretrace:", err)
		os.Exit(1)
	}
}

func run(pcap, ports, events string, round int, allowIncomplete bool) error {
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
	trace, done, err := wiretrace.Decode(bufio.NewReaderSize(f, 1<<20), serverPorts)
	if err != nil {
		return err
	}
	if events != "" {
		out, err := os.Create(events)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(out)
		if err := trace.WriteEvents(w); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	s := trace.Summarize()
	// A message that did not decode is as missing as one never captured.
	s.Gaps, s.Partial, s.Complete = done.Gaps, len(done.Partial), done.Complete() && s.Undecodable == 0
	if round > 0 {
		r := trace.CommitRounds(round)
		s.Rounds = &r
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	if !s.Complete && !allowIncomplete {
		return fmt.Errorf("incomplete capture (%d stream gaps, %d cut-off streams %v, %d undecodable messages); counts are not a measurement",
			done.Gaps, len(done.Partial), done.Partial, s.Undecodable)
	}
	return nil
}

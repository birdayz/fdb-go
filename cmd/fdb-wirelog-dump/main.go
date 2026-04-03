// fdb-wirelog-dump reads a binary wire log (FDB_WIRE_LOG format) and prints
// human-readable frame summaries. Use -hex to include hex dumps.
//
// Usage:
//
//	fdb-wirelog-dump [-hex] [-last N] [-send-only] <file.wirelog>
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	showHex := flag.Bool("hex", false, "show hex dump of frame body")
	lastN := flag.Int("last", 0, "show only last N frames")
	sendOnly := flag.Bool("send-only", false, "show only SEND frames")
	recvOnly := flag.Bool("recv-only", false, "show only RECV frames")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: fdb-wirelog-dump [-hex] [-last N] [-send-only] <file.wirelog>\n")
		os.Exit(1)
	}

	f, err := os.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	var frames []frame
	idx := 0
	for {
		var hdr [29]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			fmt.Fprintf(os.Stderr, "read header at frame %d: %v\n", idx, err)
			os.Exit(1)
		}
		dir := hdr[0]
		ts := time.Unix(0, int64(binary.LittleEndian.Uint64(hdr[1:9])))
		tokenHi := binary.LittleEndian.Uint64(hdr[9:17])
		tokenLo := binary.LittleEndian.Uint64(hdr[17:25])
		bodyLen := binary.LittleEndian.Uint32(hdr[25:29])

		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(f, body); err != nil {
			fmt.Fprintf(os.Stderr, "read body at frame %d (len=%d): %v\n", idx, bodyLen, err)
			os.Exit(1)
		}

		frames = append(frames, frame{dir, ts, tokenHi, tokenLo, body, idx})
		idx++
	}

	// Apply filters.
	start := 0
	if *lastN > 0 && len(frames) > *lastN {
		start = len(frames) - *lastN
	}

	for _, fr := range frames[start:] {
		if *sendOnly && fr.dir != 'S' {
			continue
		}
		if *recvOnly && fr.dir != 'R' {
			continue
		}

		d := "SEND"
		if fr.dir == 'R' {
			d = "RECV"
		}
		fmt.Printf("#%04d [%s] %s token=%016x:%016x len=%d\n",
			fr.index, d, fr.timestamp.Format("15:04:05.000000"), fr.tokenHi, fr.tokenLo, len(fr.body))

		if *showHex {
			dumped := hex.Dump(fr.body)
			if len(dumped) > 512 {
				fmt.Print(dumped[:512])
				fmt.Printf("  ... (%d more bytes)\n", len(fr.body)-256)
			} else {
				fmt.Print(dumped)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "total: %d frames (%d SEND, %d RECV)\n",
		len(frames),
		countDir(frames, 'S'),
		countDir(frames, 'R'))
}

type frame struct {
	dir       byte
	timestamp time.Time
	tokenHi   uint64
	tokenLo   uint64
	body      []byte
	index     int
}

func countDir(frames []frame, dir byte) int {
	n := 0
	for _, f := range frames {
		if f.dir == dir {
			n++
		}
	}
	return n
}

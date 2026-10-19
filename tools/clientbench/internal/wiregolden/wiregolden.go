// Package wiregolden renders the client requests of a capture as a normalized
// transcript, so that libfdb_c's and the Go client's requests for the same
// deterministic scenario can be compared line by line.
//
// A scenario starts with a read of MarkerPrefix+name, so the transcript splits
// by scenario the same way for both clients. Within a scenario, requests are in
// the order they became readable at the capture point; what differs between
// runs is normalized away: reply tokens, span and debug IDs, idempotency IDs,
// versions (numbered V1, V2, ... in order of appearance within the scenario),
// the 16 random bytes of a self-conflicting range key, and the scenario's key
// prefix (written <p>).
package wiregolden

import (
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
	"fdb.dev/tools/clientbench/internal/wiretrace"
)

// MarkerPrefix is the key a scenario's marker read gets, followed by its name.
const MarkerPrefix = "\x02wg/marker/"

type message interface{ UnmarshalFromReader(*wire.Reader) }

// requests are the client request types the transcript decodes.
var requests = map[uint32]func() message{
	types.GetValueRequestFileID:              func() message { return &types.GetValueRequest{} },
	types.GetKeyRequestFileID:                func() message { return &types.GetKeyRequest{} },
	types.GetKeyValuesRequestFileID:          func() message { return &types.GetKeyValuesRequest{} },
	types.GetMappedKeyValuesRequestFileID:    func() message { return &types.GetMappedKeyValuesRequest{} },
	types.GetReadVersionRequestFileID:        func() message { return &types.GetReadVersionRequest{} },
	types.CommitTransactionRequestFileID:     func() message { return &types.CommitTransactionRequest{} },
	types.GetKeyServerLocationsRequestFileID: func() message { return &types.GetKeyServerLocationsRequest{} },
	types.WatchValueRequestFileID:            func() message { return &types.WatchValueRequest{} },
	types.SplitRangeRequestFileID:            func() message { return &types.SplitRangeRequest{} },
	types.WaitMetricsRequestFileID:           func() message { return &types.WaitMetricsRequest{} },
	types.OpenDatabaseCoordRequestFileID:     func() message { return &types.OpenDatabaseCoordRequest{} },
}

// dropped are fields whose values identify a run, not the request.
var dropped = map[string]bool{
	"Reply": true, "SpanContext": true, "DebugID": true, "HasDebugID": true,
	"Arena": true, "IdempotencyId": true,
}

// versionFields hold versions, which are numbered per scenario.
var versionFields = map[string]bool{"Version": true, "ReadSnapshot": true, "MinKnownCommittedVersion": true}

const selfConflictPrefix = "\xff/SC/"

// Request is one decoded client request.
type Request struct {
	TimeNS int64
	Seq    int // decode order, which breaks ties between equal times
	Kind   string
	Msg    any
}

// Capture is a decoded capture.
type Capture struct {
	Requests []Request
	// Other counts client frames of file identifiers the transcript does not
	// decode. They appear in the transcript as FileID(n), without fields.
	Other map[uint32]int
	// Undecodable counts frames of a decoded type that failed to parse.
	Undecodable int
	Complete    bool
}

// Decode reads a pcap and decodes the client requests to serverPorts.
func Decode(r io.Reader, serverPorts []uint16) (*Capture, error) {
	server := map[uint16]bool{}
	for _, p := range serverPorts {
		server[p] = true
	}
	c := &Capture{Other: map[uint32]int{}}
	type connKey struct {
		flow wiretrace.Flow
		conn int
	}
	parsers := map[connKey]*wiretrace.FrameParser{}
	var parseErr error
	re := wiretrace.NewReassembler(func(flow wiretrace.Flow, ch wiretrace.Chunk) error {
		key := connKey{flow, ch.Conn}
		p := parsers[key]
		if p == nil {
			p = wiretrace.NewFrameParser(flow, func(f wiretrace.Frame) error {
				if server[f.Flow.Dst.Port] {
					c.add(f)
				}
				return nil
			})
			parsers[key] = p
		}
		if err := p.Add(ch); err != nil && parseErr == nil {
			parseErr = err
		}
		return nil
	})
	if err := wiretrace.ReadPcap(r, func(seg wiretrace.Segment) error {
		if !server[seg.Flow.Src.Port] && !server[seg.Flow.Dst.Port] {
			return nil
		}
		return re.Add(seg)
	}); err != nil {
		return nil, err
	}
	re.Finish()
	partial := 0
	for _, p := range parsers {
		if p.Finish() != nil {
			partial++
		}
	}
	c.Complete = re.Gaps == 0 && partial == 0 && c.Undecodable == 0 && parseErr == nil
	sort.SliceStable(c.Requests, func(i, j int) bool {
		a, b := c.Requests[i], c.Requests[j]
		return a.TimeNS < b.TimeNS || (a.TimeNS == b.TimeNS && a.Seq < b.Seq)
	})
	return c, parseErr
}

func (c *Capture) add(f wiretrace.Frame) {
	id, ok := fileID(f.Body)
	if !ok {
		return
	}
	mk := requests[id]
	if mk == nil {
		c.Other[id]++
		c.Requests = append(c.Requests, Request{TimeNS: f.TimeNS, Seq: len(c.Requests), Kind: fmt.Sprintf("FileID(%d)", id)})
		return
	}
	r, err := wire.NewReader(f.Body)
	if err != nil {
		c.Undecodable++
		return
	}
	m := mk()
	m.UnmarshalFromReader(r)
	c.Requests = append(c.Requests, Request{TimeNS: f.TimeNS, Seq: len(c.Requests), Kind: reflect.TypeOf(m).Elem().Name(), Msg: m})
}

// fileID returns the root file identifier of an FDB object-serialized body,
// which may carry an 8-byte protocol version prefix (wire.NewReader).
func fileID(body []byte) (uint32, bool) {
	off := 0
	if len(body) >= 16 && body[7] == 0x0F && body[6] == 0xDB {
		off = 8
	}
	if len(body) < off+8 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(body[off+4 : off+8]), true
}

// Transcript renders the requests of each scenario. Requests before the first
// marker (setup) and after a marker named "~end" are left out.
func Transcript(reqs []Request, keyPrefix string) string {
	var out strings.Builder
	var n *normalizer
	for _, r := range reqs {
		if gv, ok := r.Msg.(*types.GetValueRequest); ok && strings.HasPrefix(string(gv.Key), MarkerPrefix) {
			name := strings.TrimPrefix(string(gv.Key), MarkerPrefix)
			n = nil
			if name != "~end" {
				fmt.Fprintf(&out, "== %s\n", name)
				n = &normalizer{versions: map[int64]string{}, prefix: keyPrefix}
			}
			continue
		}
		switch {
		case n == nil:
		case r.Msg == nil:
			fmt.Fprintf(&out, "%s\n", r.Kind)
		default:
			fmt.Fprintf(&out, "%s %s\n", r.Kind, n.value("", reflect.ValueOf(r.Msg)))
		}
	}
	return out.String()
}

type normalizer struct {
	versions map[int64]string
	prefix   string
}

// version names a version by its order of appearance; 0, -1 (invalid) and
// the largest version (latest) keep their meaning.
func (n *normalizer) version(v int64) string {
	if v == 0 || v == -1 || v == 1<<63-1 {
		return fmt.Sprint(v)
	}
	s, ok := n.versions[v]
	if !ok {
		s = fmt.Sprintf("V%d", len(n.versions)+1)
		n.versions[v] = s
	}
	return s
}

func (n *normalizer) bytes(b []byte) string {
	s := string(b)
	if strings.HasPrefix(s, selfConflictPrefix) && len(s) >= len(selfConflictPrefix)+16 {
		return "'" + escape(selfConflictPrefix) + "<random>" + escape(s[len(selfConflictPrefix)+16:]) + "'"
	}
	if n.prefix != "" && strings.HasPrefix(s, n.prefix) {
		return "'<p>" + escape(s[len(n.prefix):]) + "'"
	}
	return "'" + escape(s) + "'"
}

func escape(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f && c != '\\' && c != '\'' {
			out.WriteByte(c)
		} else {
			fmt.Fprintf(&out, "\\x%02x", c)
		}
	}
	return out.String()
}

// value renders a field; zero fields are left out of structs.
func (n *normalizer) value(field string, v reflect.Value) string {
	switch v.Kind() {
	case reflect.Ptr:
		return n.value(field, v.Elem())
	case reflect.Struct:
		var parts []string
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || dropped[f.Name] || v.Field(i).IsZero() {
				continue
			}
			parts = append(parts, f.Name+"="+n.value(f.Name, v.Field(i)))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return n.bytes(v.Bytes())
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = n.value(field, v.Index(i))
		}
		return "[" + strings.Join(parts, " ") + "]"
	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			return n.bytes(b)
		}
	case reflect.Int64:
		if versionFields[field] {
			return n.version(v.Int())
		}
	}
	return fmt.Sprint(v.Interface())
}

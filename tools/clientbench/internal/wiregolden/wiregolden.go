// Package wiregolden renders the client requests of a capture as a normalized
// transcript, so that libfdb_c's and the Go client's requests for the same
// deterministic scenario can be compared line by line.
//
// A scenario starts with a read of MarkerPrefix+name, so the transcript splits
// by scenario the same way for both clients; a marker named "~end" closes the
// last one. Within a scenario, requests are in the order they became readable
// at the capture point. What differs between runs is normalized, keeping
// equality: versions become V1, V2, ... and random byte strings (the 16 bytes
// of a self-conflicting key, idempotency IDs) become R1, R2, ... in order of
// appearance within the scenario. Reply tokens, span and debug IDs are left
// out. In key fields, the scenario's key prefix is written <p>; values are
// literal.
package wiregolden

import (
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"slices"
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
	"Reply": true, "SpanContext": true, "DebugID": true, "HasDebugID": true, "Arena": true,
}

// sampledTags are the read request types whose tags are sampled at random on
// both clients (C++ sampleReadTags, NativeAPI.actor.cpp:2080-2083: about 1% of
// requests carry the read tag set, even an empty one), so their HasTags and
// Tags are left out. The GRV and commit tag fields are compared.
var sampledTags = map[string]bool{
	"GetValueRequest": true, "GetKeyRequest": true, "GetKeyValuesRequest": true,
	"GetMappedKeyValuesRequest": true, "WatchValueRequest": true,
}

// versionFields hold versions, which are numbered per scenario.
var versionFields = map[string]bool{
	"Version": true, "ReadSnapshot": true, "MinKnownCommittedVersion": true, "MaxVersion": true,
}

// keyFields hold keys, where the scenario prefix and self-conflicting keys are
// normalized. A ClearRange mutation's Param2 is a key too (see value).
var keyFields = map[string]bool{"Key": true, "Begin": true, "End": true, "Param1": true}

// randomFields hold random identities, normalized as a whole.
var randomFields = map[string]bool{"IdempotencyId": true}

// Version sentinels, fdbclient/include/fdbclient/FDBTypes.h:575-576 (invalidVersion,
// latestVersion).
const (
	invalidVersion = -1
	latestVersion  = -2
)

const clearRange = 1 // MutationRef::ClearRange

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
	c.Complete = re.Gaps == 0 && partial == 0 && c.Undecodable == 0 && parseErr == nil && len(c.Requests) > 0
	sort.SliceStable(c.Requests, func(i, j int) bool {
		a, b := c.Requests[i], c.Requests[j]
		return a.TimeNS < b.TimeNS || (a.TimeNS == b.TimeNS && a.Seq < b.Seq)
	})
	return c, parseErr
}

// DecodeWireLog decodes the sends of a binary wire log
// (transport.CaptureWireLog): the client's requests, in the order it sent them.
func DecodeWireLog(log []byte) (*Capture, error) {
	c := &Capture{Other: map[uint32]int{}}
	for len(log) > 0 {
		if len(log) < 29 {
			return nil, fmt.Errorf("wire log: truncated entry header")
		}
		dir, n := log[0], int(binary.LittleEndian.Uint32(log[25:29]))
		if len(log) < 29+n {
			return nil, fmt.Errorf("wire log: truncated entry body")
		}
		switch dir {
		case 'S':
			c.addBody(int64(binary.LittleEndian.Uint64(log[1:9])), log[29:29+n])
		case 'R':
		default:
			return nil, fmt.Errorf("wire log: direction %q", dir)
		}
		log = log[29+n:]
	}
	c.Complete = c.Undecodable == 0 && len(c.Requests) > 0
	return c, nil
}

func (c *Capture) add(f wiretrace.Frame) { c.addBody(f.TimeNS, f.Body) }

func (c *Capture) addBody(timeNS int64, body []byte) {
	id, ok := fileID(body)
	if !ok {
		c.Undecodable++
		return
	}
	mk := requests[id]
	if mk == nil {
		c.Other[id]++
		c.Requests = append(c.Requests, Request{TimeNS: timeNS, Seq: len(c.Requests), Kind: fmt.Sprintf("FileID(%d)", id)})
		return
	}
	r, err := wire.NewReader(body)
	if err != nil {
		c.Undecodable++
		return
	}
	m := mk()
	m.UnmarshalFromReader(r)
	c.Requests = append(c.Requests, Request{TimeNS: timeNS, Seq: len(c.Requests), Kind: reflect.TypeOf(m).Elem().Name(), Msg: m})
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
// marker (setup) and after the ~end marker are left out. It fails unless the
// markers are exactly want followed by ~end, on a scenario without a request,
// and on a request inside a
// scenario that the transcript does not decode (its fields would not be
// compared).
func Transcript(reqs []Request, keyPrefix string, want []string) (string, error) {
	var out strings.Builder
	var n *normalizer
	var got []string
	counts := map[string]int{}
	ended := false
	for _, r := range reqs {
		if gv, ok := r.Msg.(*types.GetValueRequest); ok && strings.HasPrefix(string(gv.Key), MarkerPrefix) {
			if ended {
				return "", fmt.Errorf("marker %q after ~end", gv.Key)
			}
			name := strings.TrimPrefix(string(gv.Key), MarkerPrefix)
			n = nil
			if name == "~end" {
				ended = true
				continue
			}
			got = append(got, name)
			fmt.Fprintf(&out, "== %s\n", name)
			n = &normalizer{versions: map[int64]string{}, random: map[string]string{}, prefix: keyPrefix}
			continue
		}
		switch {
		case n == nil:
		case r.Msg == nil:
			return "", fmt.Errorf("scenario %s: undecoded request %s", got[len(got)-1], r.Kind)
		default:
			counts[got[len(got)-1]]++
			n.skipTags = sampledTags[r.Kind]
			fmt.Fprintf(&out, "%s %s\n", r.Kind, n.value("", reflect.ValueOf(r.Msg)))
		}
	}
	if !ended {
		return "", fmt.Errorf("no ~end marker: the run did not finish")
	}
	if !slices.Equal(got, want) {
		return "", fmt.Errorf("scenarios %v, want %v", got, want)
	}
	for _, name := range got {
		if counts[name] == 0 {
			return "", fmt.Errorf("scenario %s sent no request", name)
		}
	}
	return out.String(), nil
}

type normalizer struct {
	skipTags bool // the request's read tags are sampled; see sampledTags
	versions map[int64]string
	random   map[string]string
	prefix   string
}

// version names a version by its order of appearance; 0, the sentinels and the
// largest version keep their values.
func (n *normalizer) version(v int64) string {
	if v == 0 || v == invalidVersion || v == latestVersion || v == 1<<63-1 {
		return fmt.Sprint(v)
	}
	s, ok := n.versions[v]
	if !ok {
		s = fmt.Sprintf("V%d", len(n.versions)+1)
		n.versions[v] = s
	}
	return s
}

// randomID names a random byte string by its order of appearance, so equal
// strings keep the same name wherever they occur.
func (n *normalizer) randomID(b string) string {
	if b == "" {
		return "''"
	}
	s, ok := n.random[b]
	if !ok {
		s = fmt.Sprintf("R%d", len(n.random)+1)
		n.random[b] = s
	}
	return fmt.Sprintf("<%s:%d>", s, len(b))
}

func (n *normalizer) bytes(b []byte, kind string) string {
	s := string(b)
	switch kind {
	case "random":
		return n.randomID(s)
	case "key":
		if strings.HasPrefix(s, selfConflictPrefix) && len(s) >= len(selfConflictPrefix)+16 {
			r := s[len(selfConflictPrefix) : len(selfConflictPrefix)+16]
			return "'" + escape(selfConflictPrefix) + n.randomID(r) + escape(s[len(selfConflictPrefix)+16:]) + "'"
		}
		if n.prefix != "" && strings.HasPrefix(s, n.prefix) {
			return "'<p>" + escape(s[len(n.prefix):]) + "'"
		}
	}
	return "'" + escape(s) + "'"
}

func escape(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		// < and > are escaped so literal bytes never read as <p> or <Rn:len>.
		if c := s[i]; c >= 0x20 && c < 0x7f && c != '\\' && c != '\'' && c != '<' && c != '>' {
			out.WriteByte(c)
		} else {
			fmt.Fprintf(&out, "\\x%02x", c)
		}
	}
	return out.String()
}

func fieldKind(field string) string {
	switch {
	case keyFields[field]:
		return "key"
	case randomFields[field]:
		return "random"
	}
	return ""
}

// value renders a field; zero fields are left out of structs.
func (n *normalizer) value(field string, v reflect.Value) string {
	switch v.Kind() {
	case reflect.Ptr:
		return n.value(field, v.Elem())
	case reflect.Struct:
		var parts []string
		t := v.Type()
		clear := t == reflect.TypeFor[types.MutationRef]() && v.FieldByName("MutType").Uint() == clearRange
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || dropped[f.Name] || v.Field(i).IsZero() ||
				(n.skipTags && (f.Name == "HasTags" || f.Name == "Tags")) {
				continue
			}
			name := f.Name
			if clear && name == "Param2" {
				name = "End" // a ClearRange's Param2 is its end key
			}
			parts = append(parts, f.Name+"="+n.value(name, v.Field(i)))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return n.bytes(v.Bytes(), fieldKind(field))
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
			return n.bytes(b, fieldKind(field))
		}
	case reflect.Int64:
		if versionFields[field] {
			return n.version(v.Int())
		}
	}
	return fmt.Sprint(v.Interface())
}

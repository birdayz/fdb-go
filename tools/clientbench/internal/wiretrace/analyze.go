package wiretrace

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Kind is a request type the analyzer follows.
type Kind string

const (
	KindGRV    Kind = "grv"
	KindCommit Kind = "commit"
	KindRange  Kind = "range" // storage GetKeyValues
)

// Request is a GRV or commit request and, once seen, its reply.
type Request struct {
	Kind       Kind
	Flow       Flow  // client to server
	SentNS     int64 // capture time of the request's last byte
	Segment    int   // client segment number within Flow
	TxnCount   uint32
	ReplyToken [16]byte
	ReplyNS    int64 // 0: no reply captured
	Version    int64 // read or commit version from the reply
	ErrorCode  int   // nonzero: the reply was an error
	// Range requests only: the request's parameters and the reply's shape.
	Params     string // limit, byte limit, selector offsets and read options
	ReplyBytes int    // length of the reply's packed rows
	More       bool
}

// Trace follows the requests on client connections, which are the flows to
// one of the server ports.
type Trace struct {
	serverPorts map[uint16]bool
	pending     map[[16]byte]*Request
	Requests    []*Request
	// Undecodable counts request frames with a followed file identifier that
	// did not decode, and replies that did not decode.
	Undecodable int
}

// NewTrace follows client connections to the given server ports.
func NewTrace(serverPorts []uint16) *Trace {
	t := &Trace{serverPorts: map[uint16]bool{}, pending: map[[16]byte]*Request{}}
	for _, p := range serverPorts {
		t.serverPorts[p] = true
	}
	return t
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

// Add takes the next frame in capture order.
func (t *Trace) Add(f Frame) {
	if t.serverPorts[f.Flow.Dst.Port] {
		t.addRequest(f)
		return
	}
	if req, ok := t.pending[f.Token]; ok {
		delete(t.pending, f.Token)
		t.addReply(req, f)
	}
}

func (t *Trace) addRequest(f Frame) {
	id, ok := fileID(f.Body)
	if !ok {
		return
	}
	req := &Request{Flow: f.Flow, SentNS: f.TimeNS, Segment: f.Segment}
	switch id {
	case types.GetReadVersionRequestFileID:
		r, err := wire.NewReader(f.Body)
		if err != nil {
			t.Undecodable++
			return
		}
		var m types.GetReadVersionRequest
		m.UnmarshalFromReader(r)
		req.Kind, req.TxnCount, req.ReplyToken = KindGRV, m.TransactionCount, m.Reply.Token
	case types.CommitTransactionRequestFileID:
		r, err := wire.NewReader(f.Body)
		if err != nil {
			t.Undecodable++
			return
		}
		var m types.CommitTransactionRequest
		m.UnmarshalFromReader(r)
		req.Kind, req.TxnCount, req.ReplyToken = KindCommit, 1, m.Reply.Token
	case types.GetKeyValuesRequestFileID:
		r, err := wire.NewReader(f.Body)
		if err != nil {
			t.Undecodable++
			return
		}
		var m types.GetKeyValuesRequest
		m.UnmarshalFromReader(r)
		req.Kind, req.ReplyToken = KindRange, m.Reply.Token
		req.Params = fmt.Sprintf("limit=%d bytes=%d begin=%t/%d end=%t/%d", m.Limit, m.LimitBytes,
			m.Begin.OrEqual, m.Begin.Offset, m.End.OrEqual, m.End.Offset)
		if m.HasOptions {
			req.Params += fmt.Sprintf(" type=%d cache=%t lockaware=%t", m.Options.Type, m.Options.CacheResult, m.Options.LockAware)
		}
	default:
		return
	}
	t.Requests = append(t.Requests, req)
	t.pending[req.ReplyToken] = req
}

func (t *Trace) addReply(req *Request, f Frame) {
	req.ReplyNS = f.TimeNS
	var r wire.Reader
	if err := wire.ReadErrorOrInto(f.Body, &r); err != nil {
		var fe *wire.FDBError
		if errors.As(err, &fe) {
			req.ErrorCode = fe.Code
			return
		}
		t.Undecodable++
		return
	}
	switch req.Kind {
	case KindGRV:
		var m types.GetReadVersionReply
		m.UnmarshalFromReader(&r)
		req.Version = m.Version
	case KindCommit:
		var m types.CommitID
		m.UnmarshalFromReader(&r)
		req.Version = m.Version
	case KindRange:
		var m types.GetKeyValuesReply
		m.UnmarshalFromReader(&r)
		req.Version, req.ReplyBytes, req.More = m.Version, len(m.Data), m.More
		if m.HasError {
			req.ErrorCode = int(m.Error.ErrorCode)
		}
	}
}

// KindSummary describes one request kind over a capture.
type KindSummary struct {
	Requests     int `json:"requests"`
	Replied      int `json:"replied"`
	Errors       int `json:"errors"`
	Transactions int `json:"transactions"` // GRV: transactions the requests asked for
	// ClientSegments is the number of client TCP segments that completed a
	// request: requests written together share one.
	ClientSegments int `json:"client_segments"`
	// ServerBatches is the number of distinct reply versions: the proxy gives
	// every request of one batch the same version.
	ServerBatches int `json:"server_batches"`
	// BatchSizes counts server batches by the number of requests in them.
	BatchSizes map[int]int `json:"batch_sizes"`
	// ArrivalSpreadNS is the time from a batch's first request to its last,
	// at the capture point; the median, 90th percentile and maximum.
	ArrivalSpreadNS [3]int64 `json:"arrival_spread_ns"`
	// LatencyNS is the time from request to reply at the capture point.
	LatencyNS [3]int64 `json:"latency_ns"`
	// Params counts range requests by their parameters; More counts replies
	// that reported more rows; ReplyBytes is their total packed size.
	Params     map[string]int `json:"params,omitempty"`
	More       int            `json:"more,omitempty"`
	ReplyBytes int            `json:"reply_bytes,omitempty"`
}

// Summary describes a capture.
type Summary struct {
	Flows       int                  `json:"client_flows"`
	Kinds       map[Kind]KindSummary `json:"kinds"`
	Undecodable int                  `json:"undecodable"`
	Gaps        int                  `json:"stream_gaps"`
	Rounds      *Rounds              `json:"rounds,omitempty"`
}

// Summarize groups the replied requests of each kind into server batches.
func (t *Trace) Summarize() Summary {
	s := Summary{Kinds: map[Kind]KindSummary{}, Undecodable: t.Undecodable}
	flows := map[Flow]bool{}
	for _, kind := range []Kind{KindGRV, KindCommit, KindRange} {
		ks := KindSummary{BatchSizes: map[int]int{}}
		if kind == KindRange {
			ks.Params = map[string]int{}
		}
		type segKey struct {
			flow Flow
			seg  int
		}
		segs := map[segKey]bool{}
		batches := map[int64][]*Request{}
		var latency []int64
		for _, r := range t.Requests {
			if r.Kind != kind {
				continue
			}
			flows[r.Flow] = true
			ks.Requests++
			if kind == KindRange {
				ks.Params[r.Params]++
				ks.ReplyBytes += r.ReplyBytes
				if r.More {
					ks.More++
				}
			}
			ks.Transactions += int(r.TxnCount)
			segs[segKey{r.Flow, r.Segment}] = true
			switch {
			case r.ReplyNS == 0:
			case r.ErrorCode != 0:
				ks.Replied++
				ks.Errors++
			default:
				ks.Replied++
				batches[r.Version] = append(batches[r.Version], r)
				latency = append(latency, r.ReplyNS-r.SentNS)
			}
		}
		ks.ClientSegments = len(segs)
		if kind == KindRange {
			// Storage replies carry the read version, not a batch: report no batches.
			batches = nil
		}
		ks.ServerBatches = len(batches)
		var spread []int64
		for _, b := range batches {
			ks.BatchSizes[len(b)]++
			lo, hi := b[0].SentNS, b[0].SentNS
			for _, r := range b {
				lo, hi = min(lo, r.SentNS), max(hi, r.SentNS)
			}
			spread = append(spread, hi-lo)
		}
		ks.ArrivalSpreadNS = quantiles(spread)
		ks.LatencyNS = quantiles(latency)
		s.Kinds[kind] = ks
	}
	s.Flows = len(flows)
	return s
}

// Rounds describes the commit batches of a closed loop whose rounds have the
// given size: whether each batch, in order of its first request, held a whole
// round.
type Rounds struct {
	Size int `json:"round_size"`
	// Whole and Split count the batches that did and did not hold a whole round.
	Whole int `json:"whole_batches"`
	Split int `json:"split_batches"`
	// FirstSplit is the index of the first batch after a whole one that did not
	// hold a whole round, -1 if none.
	FirstSplit int `json:"first_split"`
	// Runs alternates the lengths of consecutive whole and split stretches,
	// starting with a whole one (possibly of length 0).
	Runs []int `json:"runs"`
}

// CommitRounds classifies the replied commit batches against a round size.
func (t *Trace) CommitRounds(size int) Rounds {
	batches := map[int64]int64{} // version -> first request time
	count := map[int64]int{}
	for _, r := range t.Requests {
		if r.Kind != KindCommit || r.ReplyNS == 0 || r.ErrorCode != 0 {
			continue
		}
		if first, ok := batches[r.Version]; !ok || r.SentNS < first {
			batches[r.Version] = r.SentNS
		}
		count[r.Version]++
	}
	versions := make([]int64, 0, len(batches))
	for v := range batches {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return batches[versions[i]] < batches[versions[j]] })
	rs := Rounds{Size: size, FirstSplit: -1, Runs: []int{0}}
	seenWhole := false
	for i, v := range versions {
		whole := count[v] == size
		if whole {
			rs.Whole++
			seenWhole = true
		} else {
			rs.Split++
			if seenWhole && rs.FirstSplit < 0 {
				rs.FirstSplit = i
			}
		}
		if whole != (len(rs.Runs)%2 == 1) {
			rs.Runs = append(rs.Runs, 0)
		}
		rs.Runs[len(rs.Runs)-1]++
	}
	return rs
}

func quantiles(v []int64) [3]int64 {
	if len(v) == 0 {
		return [3]int64{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	at := func(q float64) int64 { return v[int(q*float64(len(v)-1))] }
	return [3]int64{at(0.5), at(0.9), v[len(v)-1]}
}

// WriteEvents writes one CSV line per request, in capture order.
func (t *Trace) WriteEvents(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "kind,client,segment,sent_ns,reply_ns,version,txn_count,error,reply_bytes,more,params"); err != nil {
		return err
	}
	for _, r := range t.Requests {
		if _, err := fmt.Fprintf(w, "%s,%s,%d,%d,%d,%d,%d,%d,%d,%t,%s\n", r.Kind, r.Flow.Src, r.Segment, r.SentNS, r.ReplyNS, r.Version, r.TxnCount, r.ErrorCode, r.ReplyBytes, r.More, r.Params); err != nil {
			return err
		}
	}
	return nil
}

// Decode reads a pcap capture and follows client traffic to serverPorts.
func Decode(r io.Reader, serverPorts []uint16) (*Trace, int, error) {
	t := NewTrace(serverPorts)
	parsers := map[Flow]*FrameParser{}
	var parseErr error
	re := NewReassembler(func(flow Flow, c Chunk) error {
		p := parsers[flow]
		if p == nil {
			p = NewFrameParser(flow, func(f Frame) error { t.Add(f); return nil })
			parsers[flow] = p
		}
		if err := p.Add(c); err != nil && parseErr == nil {
			parseErr = err
		}
		return nil
	})
	if err := ReadPcap(r, re.Add); err != nil {
		return nil, 0, err
	}
	return t, re.Gaps, parseErr
}

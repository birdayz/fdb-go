package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"

	"fdb.dev/layers/otel"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	maxFrameBytes  = 8 << 20
	corpusBoundary = "offline otel_fixture_v2 corpus generation: BE32-framed deterministic single-span ResourceSpans; no cluster is opened"
)

type corpusReport struct {
	reportCommon
	Frames       int64        `json:"frames"`
	CorpusBytes  int64        `json:"corpus_bytes"`
	CorpusConfig corpusConfig `json:"corpus_config"`
}

// corpusReader reads BE32-framed frames, at most limit of them.
type corpusReader struct {
	reader *bufio.Reader
	file   *os.File
	frames int
	limit  int
	ended  bool
}

func openCorpus(path string, limit int) (*corpusReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fault("corpus", "cannot open the corpus")
	}
	return &corpusReader{reader: bufio.NewReader(f), file: f, limit: limit}, nil
}

func (r *corpusReader) close() { r.file.Close() }

// next returns up to n frames; none means the corpus ended.
func (r *corpusReader) next(n int) ([][]byte, error) {
	var out [][]byte
	for range n {
		if r.ended {
			break
		}
		var size [4]byte
		if _, err := io.ReadFull(r.reader, size[:1]); err == io.EOF {
			r.ended = true
			break
		} else if err != nil {
			return nil, fault("corpus", "corpus read failed")
		}
		if _, err := io.ReadFull(r.reader, size[1:]); err != nil {
			return nil, fault("corpus", "truncated corpus frame header")
		}
		length := binary.BigEndian.Uint32(size[:])
		if length == 0 || length > maxFrameBytes || r.frames >= r.limit {
			return nil, fault("corpus", "corpus frame size or row budget exceeded")
		}
		wire := make([]byte, length)
		if _, err := io.ReadFull(r.reader, wire); err != nil {
			return nil, fault("corpus", "truncated corpus frame")
		}
		r.frames++
		out = append(out, wire)
	}
	return out, nil
}

// decodeFrame decodes one frame and requires exactly one scope with one span. Ingest's
// untimed precheck uses it, and the timed call decodes afresh with it, as Rust does.
func decodeFrame(wire []byte) (*tracepb.ResourceSpans, error) {
	var rs tracepb.ResourceSpans
	if err := proto.Unmarshal(wire, &rs); err != nil {
		return nil, fault("corpus", "frame is not an OTLP ResourceSpans")
	}
	if len(rs.ScopeSpans) != 1 || len(rs.ScopeSpans[0].Spans) != 1 || rs.ScopeSpans[0].Spans[0] == nil {
		return nil, fault("corpus", "each frame must hold exactly one scope with one span")
	}
	return &rs, nil
}

// precheckFrame is ingest's untimed admission check, the Rust driver's: one scope with
// one span, a nonzero 16-byte trace ID, a nonzero 8-byte span ID and a start time that
// fits int64. It builds no row; the timed call decodes the frame afresh.
func precheckFrame(wire []byte) error {
	rs, err := decodeFrame(wire)
	if err != nil {
		return err
	}
	sp := rs.ScopeSpans[0].Spans[0]
	nonzero := func(id []byte, n int) bool { return len(id) == n && !bytes.Equal(id, make([]byte, n)) }
	if !nonzero(sp.TraceId, 16) || !nonzero(sp.SpanId, 8) || sp.StartTimeUnixNano > math.MaxInt64 {
		return fault("corpus", "a frame needs a nonzero 16-byte trace ID, a nonzero 8-byte span ID and an int64 start time")
	}
	return nil
}

func writeFrame(w io.Writer, wire []byte) error {
	if len(wire) == 0 || len(wire) > maxFrameBytes {
		return fault("corpus", "invalid frame length")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(wire)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err := w.Write(wire)
	return err
}

// fixtureConfig is the otel_fixture_v2 generator configuration of a corpus identity.
func fixtureConfig(c corpusConfig) otel.Config {
	cfg := otel.DefaultConfig()
	cfg.Seed, cfg.Count, cfg.Tenants, cfg.Attributes, cfg.MatchEvery = c.Seed, c.Count, c.Tenants, c.Attributes, c.MatchEvery
	return cfg
}

// generateCorpus writes the new framed corpus: each frame is the fixture span's
// deterministic single-span ResourceSpans, the bytes OTEL_V1 rows store as OTLP.
func generateCorpus(ctx context.Context, j *job, r *corpusReport) error {
	out, err := newOutput(j.CorpusFile)
	if err != nil {
		return fault("corpus", "corpus_file must be a new writable file")
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	cfg := fixtureConfig(j.CorpusConfig)
	ids := map[string]bool{}
	for i := range cfg.Count {
		if err := ctx.Err(); err != nil {
			return err
		}
		span := otel.Generate(cfg, i)
		id := span.Trace + "/" + span.ID
		if ids[id] {
			return fault("corpus", "generator repeated a span identity")
		}
		ids[id] = true
		_, wire, err := otel.FixtureOTLP(span)
		if err != nil {
			return err
		}
		if err := precheckFrame(wire); err != nil {
			return err
		}
		if err := writeFrame(w, wire); err != nil {
			return err
		}
		r.Frames++
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	a, err := fileArtifact("corpus", j.CorpusFile)
	if err != nil {
		return err
	}
	r.CorpusBytes, r.CorpusSHA256 = a.Bytes, &a.SHA256
	r.Artifacts = append(r.Artifacts, a)
	if r.Frames != int64(j.ExpectedFrames) {
		return fault("corpus", "generated frames differ from expected_frames")
	}
	return nil
}

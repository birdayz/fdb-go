package recordlayer

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"os"
	"runtime"
	"testing"
	"time"
)

type largeContextCodec struct {
	name       string
	serializer *TransformedRecordSerializer
}

func largeContextCodecs() ([]largeContextCodec, error) {
	codecs := []largeContextCodec{{name: "plain"}}
	for _, level := range []int{1, 6} {
		s, err := NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).SetCompressionLevel(level).Build()
		if err != nil {
			return nil, err
		}
		codecs = append(codecs, largeContextCodec{fmt.Sprintf("zlib%d", level), s})
	}
	for _, level := range []int{1, 4} {
		s, err := NewTransformedRecordSerializerBuilder().SetGoOnlyBrotliCompression(level).Build()
		if err != nil {
			return nil, err
		}
		codecs = append(codecs, largeContextCodec{fmt.Sprintf("brotli%d", level), s})
	}
	return codecs, nil
}

func loadLargeContextCorpus() ([]byte, error) {
	const expectedSHA = "a3634fb2272e3a9a683ea856b2219700b89b09190a41809e2b240d8811f5dc4e"
	manifestBytes, err := os.ReadFile("testdata/large_context/manifest.json")
	if err != nil {
		return nil, err
	}
	var manifest struct {
		ContextTokens    int    `json:"context_tokens"`
		ContextBytes     int    `json:"context_bytes"`
		ContextSHA       string `json:"context_sha256"`
		MessagesBytes    int    `json:"messages_json_bytes"`
		MessagesSHA      string `json:"messages_json_sha256"`
		Tokenizer        string `json:"tokenizer"`
		TokenizerVersion string `json:"tokenizer_version"`
		Encoding         string `json:"encoding"`
		Sources          []struct {
			Path string `json:"path"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, err
	}
	if manifest.ContextTokens != 500000 || manifest.Tokenizer != "tiktoken" || manifest.TokenizerVersion != "0.12.0" || manifest.Encoding != "cl100k_base" || manifest.MessagesSHA != expectedSHA {
		return nil, fmt.Errorf("large context fixture does not match the pinned 500k-token corpus")
	}
	paths := make(map[string]bool)
	for _, source := range manifest.Sources {
		if source.Path == "" || paths[source.Path] {
			return nil, fmt.Errorf("empty or duplicated fixture source %q", source.Path)
		}
		paths[source.Path] = true
	}
	if len(paths) < 100 {
		return nil, fmt.Errorf("large context fixture has only %d distinct sources", len(paths))
	}
	f, err := os.Open("testdata/large_context/messages_500k.json.gz")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	data, err := io.ReadAll(io.LimitReader(gz, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if len(data) > 4<<20 || len(data) != manifest.MessagesBytes || hex.EncodeToString(sum[:]) != expectedSHA {
		return nil, fmt.Errorf("large context fixture length or SHA-256 mismatch")
	}
	var messages struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, err
	}
	if len(messages.Messages) != 2 || messages.Messages[1].Role != "user" {
		return nil, fmt.Errorf("large context fixture has the wrong message envelope")
	}
	context := []byte(messages.Messages[1].Content)
	sum = sha256.Sum256(context)
	if len(context) != manifest.ContextBytes || hex.EncodeToString(sum[:]) != manifest.ContextSHA {
		return nil, fmt.Errorf("large context content length or SHA-256 mismatch")
	}
	return data, nil
}

type largeContextCost struct {
	wall, cpu time.Duration
	allocated uint64
	mallocs   uint64
}

func largeContextMeasure(f func() ([]byte, error)) ([]byte, largeContextCost, error) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cpuBefore := largeContextCPUTime()
	start := time.Now()
	out, err := f()
	elapsed := time.Since(start)
	cpuAfter := largeContextCPUTime()
	cpu := time.Duration(-1)
	if cpuBefore >= 0 && cpuAfter >= 0 {
		cpu = cpuAfter - cpuBefore
	}
	runtime.ReadMemStats(&after)
	return out, largeContextCost{elapsed, cpu, after.TotalAlloc - before.TotalAlloc, after.Mallocs - before.Mallocs}, err
}

// Deliberately serial: allocation/CPU counters are process-wide. Other Bazel
// targets may run concurrently, so these timings are diagnostics, not an SLO.
func TestLargeContextCompression(t *testing.T) {
	corpus, err := loadLargeContextCorpus()
	if err != nil {
		t.Fatal(err)
	}
	codecs, err := largeContextCodecs()
	if err != nil {
		t.Fatal(err)
	}
	pattern := []byte("{\"role\":\"user\",\"content\":\"Explain the transaction retry and conflict rules.\"}\n")
	repetitive := bytes.Repeat(pattern, len(corpus)/len(pattern)+1)[:len(corpus)]
	random := make([]byte, len(corpus))
	rng := mathrand.New(mathrand.NewPCG(283, 500000))
	for i := range random {
		random[i] = byte(rng.Uint64())
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"distinct_code_docs_messages_500k_context_tokens", corpus},
		{"repetitive_positive_control_not_representative", repetitive},
		{"random_bytes_negative_control_not_messages", random},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Use a real protobuf bytes-field union, not precompressed fixture bytes.
			union := unionOf(string(c.data))
			wantSHA := sha256.Sum256(union)
			for _, codec := range codecs {
				encode := func() ([]byte, error) {
					if codec.serializer == nil {
						return bytes.Clone(union), nil
					}
					return codec.serializer.transform(union, nil)
				}
				warm, err := encode()
				if err != nil {
					t.Fatal(err)
				}
				if codec.serializer != nil {
					if _, err := codec.serializer.untransform(warm); err != nil {
						t.Fatal(err)
					}
				}
			}
			const rounds = 5
			for round := 0; round < rounds; round++ {
				// Rotate to reduce fixed-order bias without randomizing the corpus.
				for i := range codecs {
					codec := codecs[(i+round)%len(codecs)]
					stored, enc, err := largeContextMeasure(func() ([]byte, error) {
						if codec.serializer == nil {
							return bytes.Clone(union), nil
						}
						return codec.serializer.transform(union, nil)
					})
					if err != nil {
						t.Fatal(err)
					}
					decoded, dec, err := largeContextMeasure(func() ([]byte, error) {
						if codec.serializer == nil {
							return bytes.Clone(stored), nil
						}
						return codec.serializer.untransform(stored)
					})
					if err != nil || !bytes.Equal(decoded, union) || sha256.Sum256(decoded) != wantSHA {
						t.Fatalf("%s round %d exact round trip failed: %v", codec.name, round, err)
					}
					saving := 100 * (1 - float64(len(stored))/float64(len(union)))
					if c.name == "repetitive_positive_control_not_representative" && codec.serializer != nil && saving < 80 {
						t.Fatalf("%s positive control did not shrink: %.3f%%", codec.name, saving)
					}
					if c.name == "random_bytes_negative_control_not_messages" && codec.serializer != nil && (len(stored) != len(union)+1 || stored[0] != transformPrefixClear) {
						t.Fatalf("%s failed incompressible clear fallback", codec.name)
					}
					fmt.Printf("LARGE_CONTEXT_CODEC corpus=%s codec=%s round=%d raw_union_bytes=%d stored_bytes=%d saved_pct=%.6f encode_wall_ns=%d decode_wall_ns=%d encode_process_cpu_ns=%d decode_process_cpu_ns=%d encode_alloc_bytes=%d decode_alloc_bytes=%d encode_mallocs=%d decode_mallocs=%d sha256=%x warmup=1\n",
						c.name, codec.name, round, len(union), len(stored), saving, enc.wall.Nanoseconds(), dec.wall.Nanoseconds(), enc.cpu.Nanoseconds(), dec.cpu.Nanoseconds(), enc.allocated, dec.allocated, enc.mallocs, dec.mallocs, wantSHA)
				}
			}
		})
	}
}

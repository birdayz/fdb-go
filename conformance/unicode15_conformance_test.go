package conformance_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/antlr4-go/antlr/v4"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/keystoretest"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/semantic"
)

type unicode15JavaFixture struct {
	Schema int `json:"schema"`
	Java   struct {
		RuntimeVersion string `json:"runtimeVersion"`
		Vendor         string `json:"vendor"`
	} `json:"java"`
	RecordLayer struct {
		Coordinates string `json:"coordinates"`
		SHA256      string `json:"sha256"`
	} `json:"recordLayer"`
	Relational struct {
		Coordinates string `json:"coordinates"`
		SHA256      string `json:"sha256"`
	} `json:"relational"`
	Tokenizers []struct {
		ID        string           `json:"id"`
		Input     string           `json:"input"`
		Tokens    []string         `json:"tokens"`
		Positions map[string][]int `json:"positions"`
	} `json:"tokenizers"`
	Labels []struct {
		ID               string `json:"id"`
		Left             string `json:"left"`
		Right            string `json:"right"`
		EqualsIgnoreCase bool   `json:"equalsIgnoreCase"`
	} `json:"labels"`
	Identifiers []struct {
		ID            string `json:"id"`
		SQL           string `json:"sql"`
		Input         string `json:"input"`
		ParseAccepted bool   `json:"parseAccepted"`
		Normalized    string `json:"normalized"`
		Tokens        []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"tokens"`
	} `json:"identifiers"`
	ByteDecoding struct {
		Proto2Path   string `json:"proto2Path"`
		Proto2Schema string `json:"proto2Schema"`
		Proto3Path   string `json:"proto3Path"`
		Cases        []struct {
			ID        string `json:"id"`
			UTF8Hex   string `json:"utf8Hex"`
			ValidUTF8 bool   `json:"validUtf8"`
			Proto2    struct {
				Accepted              bool             `json:"accepted"`
				Decoded               string           `json:"decoded"`
				DecodedUTF8Hex        string           `json:"decodedUtf8Hex"`
				GeneratedDecoded      string           `json:"generatedDecoded"`
				GeneratedNameBytesHex string           `json:"generatedNameBytesHex"`
				Tokens                []string         `json:"tokens"`
				Positions             map[string][]int `json:"positions"`
			} `json:"proto2"`
			Proto3 struct {
				Accepted       bool   `json:"accepted"`
				Decoded        string `json:"decoded"`
				ExceptionClass string `json:"exceptionClass"`
				Message        string `json:"message"`
			} `json:"proto3"`
		} `json:"cases"`
	} `json:"byteDecoding"`
	Aliases []struct {
		ID               string `json:"id"`
		Input            string `json:"input"`
		StoredAlias      string `json:"storedAlias"`
		LowerEnglish     string `json:"lowerEnglish"`
		LookupOriginal   bool   `json:"lookupOriginal"`
		LookupNormalized bool   `json:"lookupNormalized"`
	} `json:"aliases"`
}

func readUnicode15JavaFixture(t *testing.T) unicode15JavaFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/unicode15_java.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture unicode15JavaFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || fixture.Java.RuntimeVersion == "" || fixture.Java.Vendor == "" ||
		fixture.RecordLayer.Coordinates != "org.foundationdb:fdb-record-layer-core:4.14.2.0" ||
		fixture.Relational.Coordinates != "org.foundationdb:fdb-relational-core:4.14.2.0" ||
		len(fixture.RecordLayer.SHA256) != 64 || len(fixture.Relational.SHA256) != 64 {
		t.Fatal("missing or incorrect Java oracle identity")
	}
	if len(fixture.Tokenizers) != 39 || len(fixture.Labels) != 17 || len(fixture.Identifiers) != 3 || len(fixture.Aliases) != 12 || len(fixture.ByteDecoding.Cases) != 10 {
		t.Fatalf("unexpected oracle populations: tokens=%d labels=%d identifiers=%d aliases=%d byteDecoding=%d",
			len(fixture.Tokenizers), len(fixture.Labels), len(fixture.Identifiers), len(fixture.Aliases), len(fixture.ByteDecoding.Cases))
	}
	return fixture
}

func unicode15TokenPositions(tokens []string) map[string][]int {
	positions := make(map[string][]int)
	for i, token := range tokens {
		positions[token] = append(positions[token], i)
	}
	return positions
}

func TestUnicode15JavaTokenizerFixtures(t *testing.T) {
	t.Parallel()
	fixture := readUnicode15JavaFixture(t)
	// These are immutable Go-v0 expectations, not inferred from either the new
	// tables or Java output. RFC-265 explicitly preserves these divergences.
	goDivergences := map[string][]string{
		"x_colon_y":               {"x:y"},
		"nfkd_sigma_boundary":     {"l·ς"},
		"nfkd_sigma_space":        {"ασ "},
		"nfkd_sigma_arabic_space": {"ασ β"},
		"jdk_cased_subset":        {"\U00010780ς"},
	}
	seenDivergences := 0
	for _, tc := range fixture.Tokenizers {
		t.Run(tc.ID, func(t *testing.T) {
			if !reflect.DeepEqual(unicode15TokenPositions(tc.Tokens), tc.Positions) {
				t.Fatal("Java tokens and Java positions disagree")
			}
			want := tc.Tokens
			if pinned, ok := goDivergences[tc.ID]; ok {
				seenDivergences++
				want = pinned
				if reflect.DeepEqual(want, tc.Tokens) {
					t.Fatal("documented Go/Java divergence unexpectedly disappeared")
				}
			}
			got, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(tc.Input, 0, recordlayer.TokenizerModeIndex)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				got = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go-v0 tokens(%q) = %q, want %q; Java = %q", tc.Input, got, want, tc.Tokens)
			}
			wantPositions := tc.Positions
			if _, differs := goDivergences[tc.ID]; differs {
				wantPositions = unicode15TokenPositions(want)
			}
			if gotPositions := unicode15TokenPositions(got); !reflect.DeepEqual(gotPositions, wantPositions) {
				t.Fatalf("positions = %v, want %v", gotPositions, wantPositions)
			}
		})
	}
	if seenDivergences != len(goDivergences) {
		t.Fatalf("exercised %d/%d pinned tokenizer divergences", seenDivergences, len(goDivergences))
	}
}

func TestUnicode15JavaProtobufByteDecodingFixtures(t *testing.T) {
	t.Parallel()
	fixture := readUnicode15JavaFixture(t)
	if fixture.ByteDecoding.Proto2Path == "" || fixture.ByteDecoding.Proto2Schema == "" || fixture.ByteDecoding.Proto3Path == "" {
		t.Fatal("byte fixture must identify the protobuf syntax and decoding paths")
	}
	invalidCases := 0
	for _, tc := range fixture.ByteDecoding.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			input, err := hex.DecodeString(tc.UTF8Hex)
			if err != nil {
				t.Fatal(err)
			}
			if utf8.Valid(input) != tc.ValidUTF8 || !tc.Proto2.Accepted || tc.Proto2.GeneratedNameBytesHex != tc.UTF8Hex ||
				tc.Proto2.Decoded != tc.Proto2.GeneratedDecoded || hex.EncodeToString([]byte(tc.Proto2.Decoded)) != tc.Proto2.DecodedUTF8Hex {
				t.Fatal("inconsistent actual Java proto2 record-field decode")
			}
			if tc.ValidUTF8 {
				if !tc.Proto3.Accepted || tc.Proto3.Decoded != string(input) || tc.Proto2.Decoded != string(input) {
					t.Fatal("valid UTF-8 must decode identically under proto2 and proto3")
				}
			} else {
				invalidCases++
				if tc.Proto3.Accepted || tc.Proto3.ExceptionClass != "com.google.protobuf.InvalidProtocolBufferException" ||
					tc.Proto3.Message != "Protocol message had invalid UTF-8." || !utf8.ValidString(tc.Proto2.Decoded) {
					t.Fatal("invalid UTF-8 must be replaced for this proto2 field, rejected for proto3")
				}
			}
			wire := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), input)
			var customer demopb.Customer
			if err := proto.Unmarshal(wire, &customer); err != nil {
				t.Fatal(err)
			}
			// Unlike Java's String view, Go proto2 retains invalid input bytes.
			// Tokenize the actual Go record field, not the Java replacement text.
			if customer.GetName() != string(input) {
				t.Fatal("Go proto2 string field no longer preserves its original bytes")
			}
			got, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(customer.GetName(), 0, recordlayer.TokenizerModeIndex)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.Proto2.Tokens) || !reflect.DeepEqual(unicode15TokenPositions(got), tc.Proto2.Positions) {
				t.Fatalf("Go raw-byte TEXT output = %q, Java decoded-string output = %q", got, tc.Proto2.Tokens)
			}
		})
	}
	if invalidCases != 6 {
		t.Fatalf("exercised %d invalid UTF-8 cases, want 6", invalidCases)
	}
}

func TestUnicode15JavaColumnLabelPolicyFixtures(t *testing.T) {
	t.Parallel()
	fixture := readUnicode15JavaFixture(t)
	// Go uses Unicode simple-fold orbits, not Java's upper-then-lower comparison.
	goDivergences := map[string]bool{"dotted_i": false, "dotless_i": false}
	seenDivergences := 0
	for _, tc := range fixture.Labels {
		t.Run(tc.ID, func(t *testing.T) {
			want := tc.EqualsIgnoreCase
			if pinned, ok := goDivergences[tc.ID]; ok {
				seenDivergences++
				want = pinned
				if want == tc.EqualsIgnoreCase {
					t.Fatal("expected distinct Java and Go simple-fold results")
				}
			}
			if got := unicode15.EqualFold(tc.Left, tc.Right); got != want {
				t.Fatalf("Go-v0 EqualFold(%q, %q) = %v, want %v; Java = %v", tc.Left, tc.Right, got, want, tc.EqualsIgnoreCase)
			}
		})
	}
	if seenDivergences != len(goDivergences) {
		t.Fatalf("exercised %d/%d simple-fold divergences", seenDivergences, len(goDivergences))
	}
}

func TestUnicode15JavaIdentifierFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range readUnicode15JavaFixture(t).Identifiers {
		t.Run(tc.ID, func(t *testing.T) {
			if !tc.ParseAccepted {
				t.Fatal("Java oracle did not accept the identifier probe")
			}
			root, err := parser.Parse(tc.SQL)
			if err != nil {
				t.Fatal(err)
			}
			var tokens []struct{ kind, text string }
			var visit func(antlr.Tree)
			visit = func(tree antlr.Tree) {
				if terminal, ok := tree.(antlr.TerminalNode); ok {
					token := terminal.GetSymbol()
					if token.GetTokenType() != antlr.TokenEOF {
						tokens = append(tokens, struct{ kind, text string }{
							root.GetParser().GetSymbolicNames()[token.GetTokenType()], token.GetText(),
						})
					}
				}
				for _, child := range tree.GetChildren() {
					visit(child)
				}
			}
			visit(root)
			if len(tokens) != len(tc.Tokens) {
				t.Fatalf("Go lexer returned %d tokens, Java %d", len(tokens), len(tc.Tokens))
			}
			for i, token := range tokens {
				if token.kind != tc.Tokens[i].Kind || token.text != tc.Tokens[i].Text {
					t.Errorf("token %d = %+v, Java = %+v", i, token, tc.Tokens[i])
				}
			}
			if got := semantic.NormalizeString(tc.Input, false); got != tc.Normalized {
				t.Fatalf("NormalizeString(%q) = %q, Java = %q", tc.Input, got, tc.Normalized)
			}
		})
	}
}

func testUnicode15KeystoreLookup(t *testing.T, input, goAlias, javaAlias string) {
	t.Helper()
	key := []byte("0123456789abcdef")
	path := filepath.Join(t.TempDir(), "unicode15.p12")
	// Use an already-lowercase alias so the test writer's casing cannot act as
	// an oracle for the production load/lookup policy being tested.
	if err := keystoretest.WritePKCS12(path, "fixture", "fixture", []keystoretest.Entry{{Alias: goAlias, Key: key}}); err != nil {
		t.Fatal(err)
	}
	lookup := func(alias string) ([]byte, error) {
		manager, err := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).
			SetKeyStorePassword("fixture").SetDefaultKeyEntryAlias(alias).Build()
		if err != nil {
			return nil, err
		}
		return manager.GetKey(0)
	}
	for _, alias := range []string{input, goAlias} {
		if got, err := lookup(alias); err != nil || !bytes.Equal(got, key) {
			t.Fatalf("Go keystore lookup(%q) = %x, %v; want %x", alias, got, err, key)
		}
	}
	if javaAlias != goAlias {
		var serializationErr *recordlayer.RecordSerializationError
		if _, err := lookup(javaAlias); !errors.As(err, &serializationErr) {
			t.Fatalf("Go lookup must not equate Java's full-case alias %q with %q: %v", javaAlias, goAlias, err)
		}
	}
}

func TestUnicode15JavaKeystoreAliasPolicyFixtures(t *testing.T) {
	t.Parallel()
	// Alias loading/lookup intentionally retains simple lowercase. Java PKCS12
	// applies full string casing, including dotted I and contextual final sigma.
	goDivergences := map[string]string{"dotted_i": "i", "sigma": "ασ"}
	seenDivergences := 0
	for _, tc := range readUnicode15JavaFixture(t).Aliases {
		t.Run(tc.ID, func(t *testing.T) {
			if !tc.LookupOriginal || !tc.LookupNormalized || tc.StoredAlias != tc.LowerEnglish {
				t.Fatal("Java PKCS12 alias observation is inconsistent")
			}
			want := tc.StoredAlias
			if pinned, ok := goDivergences[tc.ID]; ok {
				seenDivergences++
				want = pinned
				if want == tc.StoredAlias {
					t.Fatal("expected distinct full and simple lowercase aliases")
				}
			}
			if got := unicode15.ToLowerString(tc.Input); got != want {
				t.Fatalf("Go-v0 alias(%q) = %q, want %q; Java = %q", tc.Input, got, want, tc.StoredAlias)
			}
			if _, differs := goDivergences[tc.ID]; differs || tc.ID == "ascii" {
				testUnicode15KeystoreLookup(t, tc.Input, want, tc.StoredAlias)
			}
		})
	}
	if seenDivergences != len(goDivergences) {
		t.Fatalf("exercised %d/%d alias divergences", seenDivergences, len(goDivergences))
	}
}

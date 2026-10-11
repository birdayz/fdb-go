// Package securitydeps pins dependency fixes without running a Docker daemon or collector.
package securitydeps

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/moby/go-archive"
	"github.com/moby/sys/user"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestArchiveSymlinkChainStaysInsideDestination(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	victim := filepath.Join(root, "victim")
	for _, dir := range []string{dest, victim} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, header := range []*tar.Header{
		{Name: "inner", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "inner/go_up", Typeflag: tar.TypeSymlink, Linkname: ".."},
		{Name: "inner/go_up/escape", Typeflag: tar.TypeSymlink, Linkname: "../victim"},
		{Name: "inner/go_up/escape/newfile", Typeflag: tar.TypeReg, Mode: 0o644},
	} {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	// Either rejecting the link chain or extracting safely must leave the sibling untouched.
	_ = archive.Untar(&buf, dest, &archive.TarOptions{NoLchown: true})
	if _, err := os.Lstat(filepath.Join(victim, "newfile")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive escaped destination: victim/newfile stat = %v", err)
	}
}

func TestGroupParserRejectsOversizedRecord(t *testing.T) {
	t.Parallel()
	groups, err := user.ParseGroup(strings.NewReader("group:x:1000:" + strings.Repeat("member,", 10_000) + "last"))
	if err != nil || len(groups) != 1 || len(groups[0].List) != 10_001 {
		t.Fatalf("valid large group: groups = %d, err = %v", len(groups), err)
	}
	_, err = user.ParseGroup(strings.NewReader("group:x:1000:" + strings.Repeat("member,", 300_000)))
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("oversized group: want bounded scanner error, got %v", err)
	}
}

type nativeFields struct {
	Visible string `json:"visible"`
	Secret  string `json:"-"`
}

func TestCELNativeJSONSkipDirectiveHidesField(t *testing.T) {
	t.Parallel()
	env, err := cel.NewEnv(
		ext.NativeTypes(reflect.TypeFor[nativeFields](), ext.ParseStructTag("json")),
		cel.Variable("obj", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{name: "visible", expr: `obj.visible`},
		{name: "hidden", expr: `dyn(obj)["-"]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}
			program, err := env.Program(ast)
			if err != nil {
				t.Fatal(err)
			}
			value, _, err := program.Eval(map[string]any{"obj": nativeFields{Visible: "public", Secret: "private"}})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CEL exposed a json-skipped field: %v", value)
				}
			} else if err != nil || value.Value() != "public" {
				t.Fatalf("visible field = %v, err = %v", value, err)
			}
		})
	}
}

type endpointClient struct {
	Endpoint string
}

func (*endpointClient) Start(context.Context) error { return nil }
func (*endpointClient) Stop(context.Context) error  { return nil }
func (*endpointClient) UploadTraces(context.Context, []*tracepb.ResourceSpans) error {
	return nil
}

type endpointExporter struct {
	Endpoint string
}

func (*endpointExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (*endpointExporter) Shutdown(context.Context) error                             { return nil }

func assertEndpointHidden(t *testing.T, config any) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-collector")) {
		t.Fatalf("diagnostic config exposed endpoint: %s", data)
	}
}

func TestOTLPDiagnosticConfigHidesEndpoint(t *testing.T) {
	t.Parallel()
	exporter := otlptrace.NewUnstarted(&endpointClient{Endpoint: "https://private-collector/?token=secret"})
	assertEndpointHidden(t, exporter.MarshalLog())
}

func TestSDKDiagnosticConfigHidesExporterEndpoint(t *testing.T) {
	t.Parallel()
	processor := sdktrace.NewSimpleSpanProcessor(&endpointExporter{Endpoint: "https://private-collector/?token=secret"})
	defer func() {
		if err := processor.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	marshaler, ok := processor.(interface{ MarshalLog() any })
	if !ok {
		t.Fatal("span processor does not expose diagnostic config")
	}
	assertEndpointHidden(t, marshaler.MarshalLog())
}

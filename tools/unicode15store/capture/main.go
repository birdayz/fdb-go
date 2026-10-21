// capture exports raw golden states using an immutable pre-pin writer process.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

func main() {
	writer := flag.String("writer", "", "absolute path to immutable pre-pin Go 1.26.10 writer")
	out := flag.String("out", "", "output directory for raw golden JSON")
	flag.Parse()
	if !filepath.IsAbs(*writer) || *out == "" {
		fail(fmt.Errorf("-writer (absolute path) and -out are required"))
	}
	if err := capture(*writer, *out); err != nil {
		fail(err)
	}
}

func capture(writer, out string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := foundationdbtc.Terminate(ctx, container); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	cluster, err := container.ClusterFile(ctx)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "unicode15-capture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	clusterFile := filepath.Join(tmp, "fdb.cluster")
	if err := os.WriteFile(clusterFile, []byte(cluster), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	prefix := fmt.Sprintf("prepin-capture-%d", time.Now().UnixNano())
	run := func(prefix, operation string) ([]byte, error) {
		command := exec.CommandContext(ctx, writer, "-cluster-file", clusterFile, "-prefix", prefix, "-operation", operation)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		fmt.Fprint(os.Stderr, stderr.String())
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %s", operation, err, stderr.String())
		}
		if err := validateWriterOutput(output, stderr.String()); err != nil {
			return nil, err
		}
		return output, nil
	}
	write, err := run(prefix, "write")
	if err != nil {
		return err
	}
	rebuild, err := run(prefix, "rebuild")
	if err != nil {
		return err
	}
	if !bytes.Equal(write, rebuild) {
		return fmt.Errorf("rebuild changed raw write bytes")
	}
	update, err := run(prefix, "update")
	if err != nil {
		return err
	}
	rebuiltUpdate, err := run(prefix, "rebuild")
	if err != nil {
		return err
	}
	if !bytes.Equal(update, rebuiltUpdate) {
		return fmt.Errorf("rebuild changed raw update bytes")
	}
	deleted, err := run(prefix, "delete")
	if err != nil {
		return err
	}
	if _, err := run(prefix+"-direct-delete", "write"); err != nil {
		return err
	}
	direct, err := run(prefix+"-direct-delete", "delete")
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"write": write, "rebuild": rebuild, "update": update, "delete": deleted, "delete_written": direct} {
		if err := os.WriteFile(filepath.Join(out, name+".json"), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func validateWriterOutput(output []byte, stderr string) error {
	if !strings.Contains(stderr, "compiler=go1.26.10\n") {
		return fmt.Errorf("writer does not identify Go 1.26.10")
	}
	var report struct {
		Schema int               `json:"schema"`
		Stores []json.RawMessage `json:"stores"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return err
	}
	if report.Schema != 1 || len(report.Stores) != 19 {
		return fmt.Errorf("incomplete writer population: schema=%d stores=%d", report.Schema, len(report.Stores))
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

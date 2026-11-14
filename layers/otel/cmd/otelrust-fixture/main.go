// otelrust-fixture exports source-only compatibility vectors for the standalone
// Rust writer. It never connects to FoundationDB or performs ingestion.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: otelrust-fixture NEW_OUTPUT_DIRECTORY")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out string) error {
	vectors, wf, descriptors, err := fixture()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(vectors, "", "  ")
	if err != nil {
		return err
	}
	wireData, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(descriptors)
	if err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	for name, content := range map[string][]byte{"vectors.json": append(data, '\n'), "wire_vectors.json": append(wireData, '\n'), "descriptors.pb": wire} {
		if err := os.WriteFile(filepath.Join(out, name), content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

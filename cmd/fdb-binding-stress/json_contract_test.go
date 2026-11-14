//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteJSONRejectsUnsupportedValuesBeforeTouchingEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		value           any
		unsupportedType bool
	}{
		{"channel", make(chan int), true},
		{"nan", math.NaN(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "report.json")
			prior := []byte("{\"prior\":true}\n")
			if err := os.WriteFile(path, prior, 0o644); err != nil {
				t.Fatal(err)
			}
			err := writeJSON(path, tc.value)
			if err == nil || !strings.Contains(err.Error(), "marshal JSON "+path+":") {
				t.Fatalf("marshal rejection=%v", err)
			}
			if tc.unsupportedType {
				var target *json.UnsupportedTypeError
				if !errors.As(err, &target) {
					t.Fatalf("lost UnsupportedTypeError: %v", err)
				}
			} else {
				var target *json.UnsupportedValueError
				if !errors.As(err, &target) {
					t.Fatalf("lost UnsupportedValueError: %v", err)
				}
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != string(prior) {
				t.Fatalf("prior evidence=%q err=%v", got, readErr)
			}
			if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
				t.Fatalf("marshal failure touched temp destination: %v", statErr)
			}
		})
	}
}

func TestWriteJSONReplacesPathWithoutChangingHeldOldFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.json")
	prior := []byte("{\"prior\":true}\n")
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldInfo, err := old.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the held FD initially designates the live report inode.
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(oldInfo, pathInfo) {
		t.Fatalf("old FD fixture did not hold report inode: %v", err)
	}
	if err := writeJSON(path, map[string]bool{"pass": true}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "{\n  \"pass\": true\n}" {
		t.Fatalf("new report=%q err=%v", got, err)
	}
	held, err := io.ReadAll(old)
	if err != nil || string(held) != string(prior) {
		t.Fatalf("held old FD changed: %q err=%v", held, err)
	}
	newInfo, err := os.Stat(path)
	if err != nil || os.SameFile(oldInfo, newInfo) {
		t.Fatalf("report path did not move to replacement inode: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("successful atomic replacement left temp: %v", err)
	}
}

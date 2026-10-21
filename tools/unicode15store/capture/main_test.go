package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateWriterOutput(t *testing.T) {
	t.Parallel()
	valid := fmt.Sprintf(`{"schema":1,"stores":[%s]}`, strings.TrimSuffix(strings.Repeat(`{},`, 19), ","))
	for _, tc := range []struct {
		name, report, compiler string
		ok                     bool
	}{
		{"complete", valid, "compiler=go1.26.10\n", true},
		{"missing compiler", valid, "", false},
		{"wrong compiler", valid, "compiler=go1.27.3\n", false},
		{"malformed JSON", "{", "compiler=go1.26.10\n", false},
		{"empty stores", `{"schema":1,"stores":[]}`, "compiler=go1.26.10\n", false},
		{"missing stores", `{"schema":1}`, "compiler=go1.26.10\n", false},
		{"wrong schema", strings.Replace(valid, `"schema":1`, `"schema":2`, 1), "compiler=go1.26.10\n", false},
		{"partial stores", `{"schema":1,"stores":[{}]}`, "compiler=go1.26.10\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateWriterOutput([]byte(tc.report), tc.compiler); (err == nil) != tc.ok {
				t.Fatalf("validation = %v, want success=%v", err, tc.ok)
			}
		})
	}
}

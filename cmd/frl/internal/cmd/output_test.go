package cmd

import (
	"strings"
	"testing"
)

func TestValidateOutputFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		value    string
		allowed  []string
		wantErr  bool
		wantMsgs []string // substrings the error must contain
	}{
		{name: "empty is accepted (command default)", value: "", allowed: []string{"text", "json"}},
		{name: "text ok", value: "text", allowed: []string{"text", "json"}},
		{name: "json ok", value: "json", allowed: []string{"text", "json"}},
		{name: "yaml in json|yaml set", value: "yaml", allowed: []string{"json", "yaml"}},
		{
			name: "unknown rejected and lists allowed values",
			// Regression guard for copy-paste bugs: the message must quote the
			// offending input AND enumerate allowed values so the user doesn't
			// have to reach for --help.
			value:    "xml",
			allowed:  []string{"text", "json"},
			wantErr:  true,
			wantMsgs: []string{`"xml"`, "text", "json"},
		},
		{
			name: "case-sensitive — TEXT is not text",
			// If we ever want case-insensitive parsing it should be an explicit
			// decision, not a silent convenience. Lock in the current behavior.
			value:   "TEXT",
			allowed: []string{"text", "json"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOutputFormat(tc.value, tc.allowed...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				for _, sub := range tc.wantMsgs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

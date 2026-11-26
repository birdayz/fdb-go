package javayamsql

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a yamsql semantic version: either the `!current_version` sentinel
// or a literal four-part version, mirroring SemanticVersion.parse.
type Version struct {
	// Current is true for the `!current_version` tag, which the release process
	// rewrites into a literal version.
	Current bool
	Major   int
	Minor   int
	Build   int
	Patch   int
}

func (v Version) String() string {
	if v.Current {
		return "!current_version"
	}
	return fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Build, v.Patch)
}

// parseVersion mirrors PreambleBlock.parseVersion: the `!current_version` tag,
// or a string. Anything else is an error, which is what makes the
// supported-version/illegal-version-at-* corpus files parse-level negatives.
func parseVersion(v *Value) (*Version, error) {
	if v == nil {
		return nil, fmt.Errorf("missing version")
	}
	if v.TagName() == TagCurrentVersion {
		return &Version{Current: true}, nil
	}
	// SemanticVersion.parse takes a String; a bare 4.0.559.0 in YAML resolves to
	// a string anyway (two dots make it non-numeric), but an unquoted 3.0 would
	// resolve to a float and must be rejected rather than coerced.
	u := v.Untag()
	if u == nil || u.Kind != KindString {
		return nil, fmt.Errorf("line %d: unable to determine semantic version from %s", v.Line, kindOf(v))
	}
	return parseVersionString(u.Str, v.Line)
}

func parseVersionString(s string, line int) (*Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return nil, fmt.Errorf("line %d: invalid version %q, expected <major>.<minor>.<build>.<patch>", line, s)
	}
	out := &Version{}
	dst := [...]*int{&out.Major, &out.Minor, &out.Build, &out.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("line %d: invalid version %q: component %q is not a non-negative integer", line, s, p)
		}
		*dst[i] = n
	}
	return out, nil
}

func kindOf(v *Value) string {
	if v == nil {
		return "nothing"
	}
	if v.Kind == KindTagged {
		return v.Tag
	}
	return v.Kind.String()
}

// parser.go — Parses FDB C++ headers to extract all protocol message definitions.
//
// Finds structs with file_identifier, extracts serializer() argument lists,
// maps field types to wire sizes/alignments, computes vtables.
//
// Outputs one JSON file per message into an output directory.
// Also outputs test vectors from a C++ binary (if provided via -test-vectors flag).

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// MessageDef describes one protocol message — written as one JSON file.
type MessageDef struct {
	Name           string     `json:"name"`
	FileIdentifier uint32     `json:"file_identifier"`
	FDBVersion     string     `json:"fdb_version"`
	ReplyType      string     `json:"reply_type,omitempty"`
	SourceFile     string     `json:"source_file"`
	VTable         []uint16   `json:"vtable"`
	Fields         []FieldDef `json:"fields"`
}

// FieldDef describes one field in a message's serializer() call.
type FieldDef struct {
	Name          string `json:"name"`
	CppType       string `json:"cpp_type"`
	Type          string `json:"type"`
	VTableSlot    int    `json:"vtable_slot"`
	WireSize      uint32 `json:"wire_size"`
	WireAlignment uint32 `json:"wire_alignment"`
	Inline        bool   `json:"inline"`
}

// typeInfo describes the wire characteristics of a C++ type.
type typeInfo struct {
	category string // "scalar", "bytes", "vector", "optional", "struct", "arena"
	size     uint32
	align    uint32
}

// knownTypes maps C++ type names to their wire characteristics.
// Types not in this map are treated as "struct" (expect_serialize_member → RelativeOffset).
var knownTypes = map[string]typeInfo{
	// Scalars
	"bool":            {category: "scalar", size: 1, align: 1},
	"int8_t":          {category: "scalar", size: 1, align: 1},
	"uint8_t":         {category: "scalar", size: 1, align: 1},
	"int16_t":         {category: "scalar", size: 2, align: 2},
	"uint16_t":        {category: "scalar", size: 2, align: 2},
	"int":             {category: "scalar", size: 4, align: 4},
	"int32_t":         {category: "scalar", size: 4, align: 4},
	"uint32_t":        {category: "scalar", size: 4, align: 4},
	"int64_t":         {category: "scalar", size: 8, align: 8},
	"uint64_t":        {category: "scalar", size: 8, align: 8},
	"double":          {category: "scalar", size: 8, align: 8},
	"Version":         {category: "scalar", size: 8, align: 8},
	"Generation":      {category: "scalar", size: 8, align: 8},
	"LogEpoch":        {category: "scalar", size: 8, align: 8},
	"Sequence":        {category: "scalar", size: 8, align: 8},
	"DBRecoveryCount": {category: "scalar", size: 8, align: 8},

	// Enums (default to their common underlying types)
	"TransactionPriority": {category: "scalar", size: 1, align: 1},
	"ClusterType":         {category: "scalar", size: 1, align: 1},
	"ReadType":            {category: "scalar", size: 1, align: 1},
	"TraceFlags":          {category: "scalar", size: 1, align: 1},
	"TaskPriority":        {category: "scalar", size: 8, align: 8},

	// Zero-size (Arena)
	"Arena": {category: "arena", size: 0, align: 0},

	// Dynamic size → RelativeOffset in vtable
	"Key":              {category: "bytes", size: 4, align: 4},
	"KeyRef":           {category: "bytes", size: 4, align: 4},
	"Value":            {category: "bytes", size: 4, align: 4},
	"ValueRef":         {category: "bytes", size: 4, align: 4},
	"StringRef":        {category: "bytes", size: 4, align: 4},
	"KeyRange":         {category: "bytes", size: 4, align: 4},
	"KeyRangeRef":      {category: "bytes", size: 4, align: 4},
	"TagSet":           {category: "bytes", size: 4, align: 4},
	"VersionVector":    {category: "bytes", size: 4, align: 4},
	"IdempotencyIdRef": {category: "bytes", size: 4, align: 4},
	"HealthMetrics":    {category: "bytes", size: 4, align: 4},

	// std::string
	"std::string": {category: "bytes", size: 4, align: 4},
	"string":      {category: "bytes", size: 4, align: 4},
}

// resolveType returns the typeInfo for a C++ type string.
func resolveType(cppType string) typeInfo {
	cppType = strings.TrimSpace(cppType)

	if strings.HasPrefix(cppType, "Standalone<") {
		inner := cppType[len("Standalone<") : len(cppType)-1]
		return resolveType(inner)
	}
	if strings.HasPrefix(cppType, "Optional<") {
		return typeInfo{category: "optional", size: 0, align: 0}
	}
	if strings.HasPrefix(cppType, "VectorRef<") ||
		strings.HasPrefix(cppType, "std::vector<") ||
		strings.HasPrefix(cppType, "vector<") ||
		strings.HasPrefix(cppType, "std::deque<") {
		return typeInfo{category: "vector", size: 4, align: 4}
	}
	if strings.HasPrefix(cppType, "std::map<") ||
		strings.HasPrefix(cppType, "std::unordered_map<") ||
		strings.HasPrefix(cppType, "TransactionTagMap<") ||
		strings.HasPrefix(cppType, "UIDTransactionTagMap<") ||
		strings.HasPrefix(cppType, "boost::container::flat_map<") ||
		strings.HasPrefix(cppType, "std::set<") {
		return typeInfo{category: "vector", size: 4, align: 4}
	}
	if strings.HasPrefix(cppType, "std::pair<") {
		return typeInfo{category: "struct", size: 4, align: 4}
	}
	if strings.HasPrefix(cppType, "ReplyPromise<") ||
		strings.HasPrefix(cppType, "PublicRequestStream<") ||
		strings.HasPrefix(cppType, "RequestStream<") ||
		strings.HasPrefix(cppType, "ReplyPromiseStream<") ||
		strings.HasPrefix(cppType, "CachedSerialization<") {
		return typeInfo{category: "struct", size: 4, align: 4}
	}
	if strings.HasPrefix(cppType, "std::variant<") {
		return typeInfo{category: "optional", size: 0, align: 0}
	}
	if ti, ok := knownTypes[cppType]; ok {
		return ti
	}
	return typeInfo{category: "struct", size: 4, align: 4}
}

// parsedStruct holds raw extraction from C++ header.
type parsedStruct struct {
	name           string
	fileIdentifier uint32
	serializerArgs []string
	replyType      string
	sourceFile     string
	baseClasses    []string
}

var (
	reFileID     = regexp.MustCompile(`constexpr\s+static\s+FileIdentifier\s+file_identifier\s*=\s*(\d+)`)
	reSerializer = regexp.MustCompile(`serializer\s*\(\s*ar\s*,\s*(.+?)\s*\)`)
	reStructDecl = regexp.MustCompile(`(?:struct|class)\s+(\w+)\s*(?::(?:\s*public)?\s*(.+?))?\s*\{`)
	reReplyField = regexp.MustCompile(`ReplyPromise<(\w+)>`)
)

func parseHeaders(srcDir string) ([]parsedStruct, error) {
	var results []parsedStruct

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == "build" || base == "contrib" || base == "bindings" ||
				base == "documentation" || base == "packaging" || base == "tests" ||
				base == "recipes" || base == "design" || base == "metacluster" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".h" && !strings.HasSuffix(path, ".actor.h") {
			return nil
		}
		structs, parseErr := parseFile(path, srcDir)
		if parseErr != nil {
			return nil
		}
		results = append(results, structs...)
		return nil
	})

	return results, err
}

func parseFile(path, srcDir string) ([]parsedStruct, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	relPath, _ := filepath.Rel(srcDir, path)

	var results []parsedStruct
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	for i := 0; i < len(lines); i++ {
		m := reStructDecl.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		structName := m[1]
		baseClassStr := strings.TrimSpace(m[2])

		var fileID uint32
		var serArgs []string
		var replyType string
		braceDepth := 0
		foundFileID := false

		for j := i; j < len(lines); j++ {
			line := lines[j]
			braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
			if braceDepth <= 0 && j > i {
				break
			}
			if fm := reFileID.FindStringSubmatch(line); fm != nil {
				id, _ := strconv.ParseUint(fm[1], 10, 32)
				fileID = uint32(id)
				foundFileID = true
			}
			if sm := reSerializer.FindStringSubmatch(line); sm != nil {
				args := parseSerializerArgs(sm[1])
				serArgs = append(serArgs, args...)
			}
			if rm := reReplyField.FindStringSubmatch(line); rm != nil {
				replyType = rm[1]
			}
		}

		if foundFileID && len(serArgs) > 0 {
			var bases []string
			if baseClassStr != "" {
				for _, b := range strings.Split(baseClassStr, ",") {
					b = strings.TrimSpace(b)
					b = strings.TrimPrefix(b, "public ")
					b = strings.TrimSpace(b)
					if b != "" {
						bases = append(bases, b)
					}
				}
			}
			results = append(results, parsedStruct{
				name:           structName,
				fileIdentifier: fileID,
				serializerArgs: serArgs,
				replyType:      replyType,
				sourceFile:     relPath,
				baseClasses:    bases,
			})
		}
	}

	return results, nil
}

func parseSerializerArgs(argsStr string) []string {
	var args []string
	depth := 0
	start := 0
	for i := 0; i < len(argsStr); i++ {
		switch argsStr[i] {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				arg := strings.TrimSpace(argsStr[start:i])
				if arg != "" {
					args = append(args, arg)
				}
				start = i + 1
			}
		}
	}
	last := strings.TrimSpace(argsStr[start:])
	if last != "" {
		args = append(args, last)
	}
	return args
}

func buildMessageDef(ps parsedStruct, fieldTypes map[string]string, fdbVersion string) MessageDef {
	md := MessageDef{
		Name:           ps.name,
		FileIdentifier: ps.fileIdentifier,
		FDBVersion:     fdbVersion,
		ReplyType:      ps.replyType,
		SourceFile:     ps.sourceFile,
	}

	var sizes, aligns []uint32
	var fields []FieldDef
	slot := 0

	for _, arg := range ps.serializerArgs {
		argName := arg
		if idx := strings.LastIndex(arg, "::"); idx >= 0 {
			argName = arg[idx+2:]
		}

		cppType := fieldTypes[argName]
		if cppType == "" {
			cppType = "unknown"
		}

		ti := resolveType(cppType)

		if ti.category == "optional" {
			sizes = append(sizes, 1, 4)
			aligns = append(aligns, 1, 4)
			fields = append(fields, FieldDef{
				Name:       argName,
				CppType:    cppType,
				Type:       "optional",
				VTableSlot: slot,
			})
			slot += 2
		} else if ti.size == 0 && ti.category == "arena" {
			sizes = append(sizes, 0)
			aligns = append(aligns, 0)
			fields = append(fields, FieldDef{
				Name:       argName,
				CppType:    cppType,
				Type:       "arena",
				VTableSlot: -1,
			})
		} else {
			sizes = append(sizes, ti.size)
			aligns = append(aligns, ti.align)
			fields = append(fields, FieldDef{
				Name:          argName,
				CppType:       cppType,
				Type:          ti.category,
				VTableSlot:    slot,
				WireSize:      ti.size,
				WireAlignment: ti.align,
				Inline:        ti.category == "scalar",
			})
			slot++
		}
	}

	md.VTable = wire.GenerateVTable(sizes, aligns)
	md.Fields = fields
	return md
}

func extractFieldTypes(path string) map[string]map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	result := make(map[string]map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	var currentStruct string
	braceDepth := 0

	reField := regexp.MustCompile(`^\s+([\w:<>, ]+?)\s+(\w+)\s*[=;{]`)

	for _, line := range lines {
		if m := reStructDecl.FindStringSubmatch(line); m != nil {
			currentStruct = m[1]
			if result[currentStruct] == nil {
				result[currentStruct] = make(map[string]string)
			}
		}

		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")

		if currentStruct != "" && braceDepth > 0 {
			if m := reField.FindStringSubmatch(line); m != nil {
				fieldType := strings.TrimSpace(m[1])
				fieldName := m[2]
				if isValidFieldType(fieldType) {
					result[currentStruct][fieldName] = fieldType
				}
			}
		}

		if braceDepth <= 0 {
			currentStruct = ""
		}
	}

	return result
}

func isValidFieldType(t string) bool {
	skip := []string{"template", "void", "static", "constexpr", "explicit",
		"friend", "using", "typedef", "return", "if", "for", "while",
		"auto", "const", "virtual", "inline"}
	for _, s := range skip {
		if t == s || strings.HasPrefix(t, s+" ") {
			return false
		}
	}
	return true
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <fdb-source-dir> <output-dir>\n", os.Args[0])
		os.Exit(1)
	}
	srcDir := os.Args[1]
	outDir := os.Args[2]

	const fdbVersion = "7.3.75"

	// Parse all headers.
	structs, err := parseHeaders(srcDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing headers: %v\n", err)
		os.Exit(1)
	}

	// Build field type maps.
	fileFieldTypes := make(map[string]map[string]map[string]string)
	seenFiles := make(map[string]bool)
	for _, s := range structs {
		fullPath := filepath.Join(srcDir, s.sourceFile)
		if !seenFiles[fullPath] {
			seenFiles[fullPath] = true
			ft := extractFieldTypes(fullPath)
			fileFieldTypes[fullPath] = ft
		}
	}

	// Create output directory.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output dir: %v\n", err)
		os.Exit(1)
	}

	// Write one JSON file per message.
	for _, s := range structs {
		fullPath := filepath.Join(srcDir, s.sourceFile)
		fieldTypes := make(map[string]string)

		if ft, ok := fileFieldTypes[fullPath]; ok {
			if m, ok := ft[s.name]; ok {
				for k, v := range m {
					fieldTypes[k] = v
				}
			}
			for _, base := range s.baseClasses {
				if m, ok := ft[base]; ok {
					for k, v := range m {
						fieldTypes[k] = v
					}
				}
			}
		}

		md := buildMessageDef(s, fieldTypes, fdbVersion)

		data, err := json.MarshalIndent(md, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling %s: %v\n", s.name, err)
			continue
		}
		data = append(data, '\n')

		outPath := filepath.Join(outDir, s.name+".json")
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", outPath, err)
			continue
		}
	}

	fmt.Fprintf(os.Stderr, "Wrote %d message schemas to %s\n", len(structs), outDir)
}

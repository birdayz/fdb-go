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
	reSerializer = regexp.MustCompile(`serializer\s*\(\s*ar\s*,\s*(.+)\s*\)\s*;`)
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

		var serializerAccum string // accumulates multi-line serializer() calls

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

			// Handle multi-line serializer() calls.
			// Start accumulating when we see "serializer(" and stop at matching ")".
			if serializerAccum == "" {
				if idx := strings.Index(line, "serializer("); idx >= 0 {
					serializerAccum = line[idx:]
				}
			} else {
				serializerAccum += " " + strings.TrimSpace(line)
			}
			if serializerAccum != "" {
				// Check if we have balanced parentheses.
				depth := 0
				for _, ch := range serializerAccum {
					if ch == '(' {
						depth++
					} else if ch == ')' {
						depth--
					}
				}
				if depth <= 0 {
					// Complete serializer call — extract args.
					if sm := reSerializer.FindStringSubmatch(serializerAccum); sm != nil {
						args := parseSerializerArgs(sm[1])
						serArgs = append(serArgs, args...)
					}
					serializerAccum = ""
				}
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
		// Strip base class prefix: "LoadBalancedReply::penalty" → "penalty"
		if idx := strings.LastIndex(arg, "::"); idx >= 0 {
			argName = arg[idx+2:]
		}
		// Strip array subscript: "part[0]" → "part"
		lookupName := argName
		if idx := strings.Index(lookupName, "["); idx >= 0 {
			lookupName = lookupName[:idx]
		}

		cppType := fieldTypes[lookupName]
		if cppType == "" {
			// Well-known base class fields that our parser can't resolve
			// (declared in a different file from the struct).
			cppType = wellKnownFieldType(lookupName)
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

	// Match field declarations, handling:
	//   Type name;
	//   Type name = value;
	//   Type name1, name2;       (multi-field)
	//   Type name1, name2 = v;
	//   Type name[N];            (arrays)
	//   Optional<Type> name;     (templates)
	reFieldLine := regexp.MustCompile(`^\s+([\w:<>\s]+?)\s+([\w\[\],\s]+)\s*[;={]`)

	for _, line := range lines {
		if m := reStructDecl.FindStringSubmatch(line); m != nil {
			currentStruct = m[1]
			if result[currentStruct] == nil {
				result[currentStruct] = make(map[string]string)
			}
		}

		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")

		if currentStruct != "" && braceDepth > 0 {
			// Try to extract field type from the line.
			line = strings.TrimSpace(line)

			// Skip non-field lines.
			if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") ||
				strings.HasPrefix(line, "template") || strings.HasPrefix(line, "void ") ||
				strings.HasPrefix(line, "static ") || strings.HasPrefix(line, "constexpr") ||
				strings.HasPrefix(line, "explicit") || strings.HasPrefix(line, "friend") ||
				strings.HasPrefix(line, "using ") || strings.HasPrefix(line, "typedef") ||
				strings.HasPrefix(line, "return") || strings.HasPrefix(line, "if ") ||
				strings.HasPrefix(line, "for ") || strings.HasPrefix(line, "while") ||
				strings.HasPrefix(line, "auto ") || strings.HasPrefix(line, "virtual") ||
				strings.HasPrefix(line, "inline ") || strings.HasPrefix(line, "#") ||
				strings.HasPrefix(line, "enum ") || strings.HasPrefix(line, "struct ") ||
				strings.HasPrefix(line, "class ") {
				continue
			}

			if m := reFieldLine.FindStringSubmatch("\t" + line); m != nil {
				fieldType := strings.TrimSpace(m[1])
				namesStr := strings.TrimSpace(m[2])

				if !isValidFieldType(fieldType) {
					continue
				}

				// Handle comma-separated names: "Type name1, name2, name3"
				for _, name := range strings.Split(namesStr, ",") {
					name = strings.TrimSpace(name)
					// Strip initializer: "name = value"
					if eqIdx := strings.Index(name, "="); eqIdx >= 0 {
						name = strings.TrimSpace(name[:eqIdx])
					}
					// Strip array subscript: "name[N]" → "name"
					if brIdx := strings.Index(name, "["); brIdx >= 0 {
						name = name[:brIdx]
					}
					name = strings.TrimSpace(name)
					if name != "" && isIdentifier(name) {
						result[currentStruct][name] = fieldType
					}
				}
			}
		}

		if braceDepth <= 0 {
			currentStruct = ""
		}
	}

	return result
}

// wellKnownFieldType returns the C++ type for fields that appear in base classes
// declared in separate files. These are the most common cross-file field references.
func wellKnownFieldType(fieldName string) string {
	known := map[string]string{
		// LoadBalancedReply fields
		"penalty": "double",
		"error":   "Optional<Error>",

		// BasicLoadBalancedReply fields
		"processBusyTime": "int",

		// ReplyPromiseStreamReply fields
		"acknowledgeToken": "uint64_t",
		"sequence":         "int64_t",

		// LocalityData fields (in Interface types)
		"locality": "LocalityData",

		// Common fields
		"uniqueID":  "UID",
		"processId": "Optional<Key>",

		// StorageServerInterface serialized fields
		"getValue":          "RequestStream",
		"tssPairID":         "Optional<UID>",
		"acceptingRequests": "bool",

		// CommitProxyInterface / GrvProxyInterface
		"provisional":              "bool",
		"commit":                   "RequestStream",
		"getConsistentReadVersion": "RequestStream",

		// Common request fields
		"arena":       "Arena",
		"debugID":     "Optional<UID>",
		"spanContext": "SpanContext",
		"tenantInfo":  "TenantInfo",
		"reply":       "ReplyPromise",

		// GetKeyValuesRequest
		"limit":      "int",
		"limitBytes": "int",

		// Complex template fields (typed as vector/bytes in Go)
		"results":           "std::vector<std::pair<KeyRangeRef, std::vector<StorageServerInterface>>>",
		"resultsTssMapping": "std::vector<std::pair<UID, StorageServerInterface>>",
		"resultsTagMapping": "std::vector<std::pair<UID, Tag>>",
		"data":              "VectorRef<KeyValueRef>",
	}
	if t, ok := known[fieldName]; ok {
		return t
	}
	return "unknown"
}

func isIdentifier(s string) bool {
	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				return false
			}
		}
	}
	return len(s) > 0
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

// generateCppTestFile emits a C++ source file that serializes ALL parsed message
// structs using FDB's flat_buffers templates. The generated file includes the
// fdb_stubs.h types and the extracted struct definitions with their serialize() methods.
func generateCppTestFile(structs []parsedStruct, _ map[string]map[string]map[string]string, _ string, outPath string) error {
	var b strings.Builder

	// Header: real FDB includes only. No stubs, no custom types.
	b.WriteString(`// GENERATED — do not edit. Run: just wire-schema
// Compiled inside foundationdb/build:rockylinux9-latest with real FDB libs.
// Default-constructs each message and serializes with ObjectWriter.

// Client headers
#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/ClusterInterface.h"
#include "fdbclient/BlobWorkerInterface.h"
#include "fdbclient/EncryptKeyProxyInterface.h"
#include "fdbclient/ClientWorkerInterface.h"
#include "fdbclient/ProcessInterface.h"
#include "fdbclient/RestoreInterface.h"
#include "fdbclient/Tenant.h"
#include "fdbclient/FDBTypes.h"
#include "fdbclient/GlobalConfig.h"
#include "fdbclient/Audit.h"
#include "fdbclient/BlobGranuleCommon.h"
#include "fdbclient/BlobMetadataUtils.h"
#include "fdbclient/BlobCipher.h"
#include "fdbclient/StorageCheckpoint.h"
#include "fdbclient/StorageServerShard.h"
#include "fdbclient/MetaclusterRegistration.h"
#include "fdbclient/StorageWiggleMetrics.actor.h"
#include "fdbclient/ConsistencyScanInterface.actor.h"
#include "fdbclient/DataDistributionConfig.actor.h"

// Server headers
#include "fdbserver/WorkerInterface.actor.h"
#include "fdbserver/TLogInterface.h"
#include "fdbserver/MasterInterface.h"
#include "fdbserver/ResolverInterface.h"
#include "fdbserver/DataDistributorInterface.h"
#include "fdbserver/RatekeeperInterface.h"
#include "fdbserver/BlobManagerInterface.h"
#include "fdbserver/BlobMigratorInterface.h"
#include "fdbserver/CoordinationInterface.h"
#include "fdbserver/RestoreWorkerInterface.actor.h"
#include "fdbserver/RestoreUtil.h"
#include "fdbserver/LogSystemConfig.h"
#include "fdbserver/NetworkTest.h"
#include "fdbserver/ServerDBInfo.actor.h"
#include "fdbserver/TesterInterface.actor.h"
#include "fdbserver/KmsConnectorInterface.h"
#include "fdbserver/SimEncryptKmsProxy.actor.h"
#include "fdbserver/RocksDBCheckpointUtils.actor.h"
#include "fdbserver/RemoteIKeyValueStore.actor.h"
#include "fdbserver/StorageServerUtils.h"
#include "fdbserver/BackupInterface.h"

// Runtime + serialization
#include "flow/serialize.h"
#include "flow/TLSConfig.actor.h"
#include "fdbrpc/FlowTransport.h"
#include <cstdio>
#include <sys/stat.h>

// Fork + serialize: if the child segfaults (unresolved vtable from
// server-only types), the parent continues with the next message.
#include <unistd.h>
#include <sys/wait.h>

template <class T>
void doEmit(const char* outDir, const char* name) {
    T msg{};
    ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
    wr.serialize(FileIdentifierFor<T>::value, msg);
    auto bytes = wr.toStringRef();

    char path[4096];
    snprintf(path, sizeof(path), "%s/%s.json", outDir, name);
    FILE* f = fopen(path, "w");
    if (!f) { perror(path); return; }
    fprintf(f, "{\n  \"name\": \"%s\",\n  \"file_identifier\": %u,\n  \"size\": %d,\n  \"hex\": \"",
            name, FileIdentifierFor<T>::value, (int)bytes.size());
    for (int i = 0; i < bytes.size(); i++) fprintf(f, "%02x", bytes[i]);
    fprintf(f, "\"\n}\n");
    fclose(f);
}

// Zero-init version: bypasses constructors that crash due to unresolved vtables.
template <class T>
void doEmitZero(const char* outDir, const char* name) {
    alignas(T) char storage[sizeof(T)] = {};
    T& msg = *reinterpret_cast<T*>(storage);
    ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
    wr.serialize(FileIdentifierFor<T>::value, msg);
    auto bytes = wr.toStringRef();

    char path[4096];
    snprintf(path, sizeof(path), "%s/%s.json", outDir, name);
    FILE* f = fopen(path, "w");
    if (!f) { perror(path); return; }
    fprintf(f, "{\n  \"name\": \"%s\",\n  \"file_identifier\": %u,\n  \"size\": %d,\n  \"hex\": \"",
            name, FileIdentifierFor<T>::value, (int)bytes.size());
    for (int i = 0; i < bytes.size(); i++) fprintf(f, "%02x", bytes[i]);
    fprintf(f, "\"\n}\n");
    fclose(f);
}

// Fork-safe wrapper: try default construct first, fall back to zero-init.
template <class T>
void emit(const char* outDir, const char* name) {
    pid_t pid = fork();
    if (pid == 0) {
        doEmit<T>(outDir, name);
        _exit(0);
    }
    int status = 0;
    waitpid(pid, &status, 0);
    if (WIFEXITED(status) && WEXITSTATUS(status) == 0) {
        return; // default construction worked
    }
    // Default construction crashed (Interface types with NetNotifiedQueue vtables).
    // Retry with zero-init — bypasses constructors, serialization only reads data fields.
    pid = fork();
    if (pid == 0) {
        doEmitZero<T>(outDir, name);
        _exit(0);
    }
    waitpid(pid, &status, 0);
    if (WIFEXITED(status) && WEXITSTATUS(status) == 0) {
        fprintf(stderr, "ZERO-INIT %s\n", name);
    } else {
        fprintf(stderr, "SKIP %s (both methods failed)\n", name);
    }
}

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "Usage: %s <output-dir>\n", argv[0]); return 1; }
    const char* outDir = argv[1];
    mkdir(outDir, 0755);

    // Initialize FDB runtime (needed for ReplyPromise/TimedRequest constructors).
    TLSConfig tlsConfig;
    g_network = newNet2(tlsConfig, false, false);
    FlowTransport::createInstance(false, 1, WLTOKEN_FIRST_AVAILABLE, nullptr);

`)

	// Skip nested types (parser extracted them but they're not top-level).
	skipTypes := map[string]bool{
		"TagInfo": true, // nested inside StorageQueuingMetricsReply
	}

	emitCount := 0
	for _, s := range structs {
		if skipTypes[s.name] {
			continue
		}
		if s.fileIdentifier >= (1 << 24) {
			continue // composed file identifiers
		}
		fmt.Fprintf(&b, "    emit<%s>(outDir, \"%s\");\n", s.name, s.name)
		emitCount++
	}

	fmt.Fprintf(&b, "\n    fprintf(stderr, \"Wrote %d test vectors to %%s\\n\", outDir);\n", emitCount)
	b.WriteString("    return 0;\n}\n")

	return os.WriteFile(outPath, []byte(b.String()), 0o644)
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <fdb-source-dir> <output-dir> [--gen-cpp=<path>]\n", os.Args[0])
		os.Exit(1)
	}
	srcDir := os.Args[1]
	outDir := os.Args[2]

	var genCppPath string
	for _, arg := range os.Args[3:] {
		if strings.HasPrefix(arg, "--gen-cpp=") {
			genCppPath = arg[len("--gen-cpp="):]
		}
	}

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

	// Write one JSON schema file per message.
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

	// Optionally generate C++ test file.
	if genCppPath != "" {
		if err := generateCppTestFile(structs, fileFieldTypes, srcDir, genCppPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating C++ test file: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Generated C++ test file: %s (%d messages)\n", genCppPath, len(structs))
	}
}

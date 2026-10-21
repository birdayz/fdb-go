#!/usr/bin/env python3
"""Import and format the pinned, scoped x/text runtime snapshot."""

import argparse
import hashlib
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import zipfile


MODULE = "golang.org/x/text"
VERSION = "v0.42.0"
LOCAL = "fdb.dev/internal/unicode15/text"
ARCHIVE_SHA256 = "a7b64e003056b6470303f408202098d8f3714a115f23091b8cac85edeb265476"
ROOT = Path(__file__).resolve().parent
PACKAGES = {
    "collate",
    "internal/colltab",
    "internal/language",
    "internal/language/compact",
    "internal/tag",
    "language",
    "transform",
    "unicode/norm",
}


def replace(text, old, new, count=1):
    actual = text.count(old)
    if actual != count:
        raise ValueError(f"expected {count} occurrences of {old!r}, found {actual}")
    return text.replace(old, new)


def localize(path, data):
    if not path.endswith(".go"):
        return data
    text = data.decode("utf-8")
    # Canonical import comments and code samples name the local packages too.
    text = text.replace(f'"{MODULE}/', f'"{LOCAL}/')
    # The upstream generator programs and tests are deliberately not included.
    text = re.sub(r"(?m)^\n?(?://go:generate[^\n]*\n)+", "", text)
    if path == "unicode/norm/tables15.0.0.go":
        text = replace(text, "//go:build !go1.27\n\n", "")
    elif path == "collate/collate.go":
        text = replace(text, "\t// Process primary level\n", "\t// Process primary level\n\t// TODO: handle shifted\n")
        text = replace(text, "\t} else {\n\t\t// TODO: handle shifted\n\t}\n", "\t}\n")
    elif path == "internal/colltab/collelem.go":
        text = replace(
            text,
            '\t"fmt"\n\t"unicode"\n',
            '\t"fmt"\n\t"unicode/utf8"\n\n\tunicode15 "fdb.dev/internal/unicode15"\n',
        )
        text = replace(text, "unicode.MaxRune", "utf8.MaxRune")
        text = replace(text, "unicode.Is(unicode.Ideographic, r)", "unicode15.Is(unicode15.Ideographic, r)")
        text = replace(
            text,
            "\tmaxUnified            = 0x9FFF\n"
            "\tminCompatibility      = 0xF900\n"
            "\tmaxCompatibility      = 0xFAFF\n"
            "\tminRare               = 0x3400\n"
            "\tmaxRare               = 0x4DBF\n",
            "\tmaxUnified       rune = 0x9FFF\n"
            "\tminCompatibility rune = 0xF900\n"
            "\tmaxCompatibility rune = 0xFAFF\n"
            "\tminRare          rune = 0x3400\n"
            "\tmaxRare          rune = 0x4DBF\n",
        )
    elif path == "internal/colltab/numeric.go":
        text = replace(
            text,
            '\t"unicode"\n\t"unicode/utf8"\n',
            '\t"unicode/utf8"\n\n\tunicode15 "fdb.dev/internal/unicode15"\n',
        )
        text = replace(text, "unicode.In(r, unicode.Nd)", "unicode15.In(r, unicode15.Nd)", 2)
    elif path == "internal/language/language.go":
        text = replace(
            text,
            '\t"strings"\n',
            '\t"strings"\n\n\tunicode15 "fdb.dev/internal/unicode15"\n',
        )
        text = replace(text, "strings.ToLower(s)", "unicode15.ToLowerString(s)")
    elif path == "internal/language/compact/language.go":
        text = replace(
            text,
            f'\t"{LOCAL}/internal/language"\n',
            f'\tunicode15 "fdb.dev/internal/unicode15"\n\t"{LOCAL}/internal/language"\n',
        )
        text = replace(text, "strings.ToLower(loc.RegionID.String())", "unicode15.ToLowerString(loc.RegionID.String())")
    elif path == "language/match.go":
        text = replace(text, '\t"strings"\n', "")
        text = replace(
            text,
            f'\t"{LOCAL}/internal/language"\n',
            f'\tunicode15 "fdb.dev/internal/unicode15"\n\t"{LOCAL}/internal/language"\n',
        )
        text = replace(text, "strings.ToLower(r)", "unicode15.ToLowerString(r)")
    elif path == "language/parse.go":
        text = replace(
            text,
            f'\t"{LOCAL}/internal/language"\n',
            f'\tunicode15 "fdb.dev/internal/unicode15"\n\t"{LOCAL}/internal/language"\n',
        )
        text = replace(text, "strings.TrimSpace(", "trimSpace(", 4)
        text = replace(
            text,
            "// Add hack mapping to deal with a small number of cases that occur\n",
            "// trimSpace keeps locale parsing independent of the compiler's Unicode tables.\n"
            "func trimSpace(s string) string {\n"
            "\treturn strings.TrimFunc(s, func(r rune) bool {\n"
            "\t\treturn unicode15.Is(unicode15.White_Space, r)\n"
            "\t})\n"
            "}\n\n"
            "// Add hack mapping to deal with a small number of cases that occur\n",
        )
    if path == "language/coverage.go":
        text = replace(text, "interface{}", "any")
    elif path == "language/parse.go":
        text = replace(text, "interface{}", "any", 3)
    return text.encode("utf-8")


def manifest(path):
    entries = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        digest, path = line.split("  ", 1)
        parts = PurePosixPath(path).parts
        if not re.fullmatch(r"[0-9a-f]{64}", digest) or not parts or path.startswith("/") or ".." in parts:
            raise ValueError(f"invalid manifest entry: {line}")
        if path in entries:
            raise ValueError(f"duplicate manifest entry: {path}")
        entries[path] = digest
    if not entries or not {"LICENSE", "PATENTS"} <= set(entries):
        raise ValueError("manifest must contain the runtime sources, LICENSE and PATENTS")
    packages = {str(PurePosixPath(p).parent) for p in entries if p.endswith(".go")}
    if packages != PACKAGES:
        raise ValueError(f"unexpected package closure: {sorted(packages)}")
    return entries


def read_sources(source, entries):
    if source.is_dir():
        return {path: (source / path).read_bytes() for path in entries}
    archive = source.read_bytes()
    digest = hashlib.sha256(archive).hexdigest()
    if digest != ARCHIVE_SHA256:
        raise ValueError(f"module archive SHA-256 mismatch: {digest}")
    with zipfile.ZipFile(source) as z:
        return {path: z.read(f"{MODULE}@{VERSION}/{path}") for path in entries}


def format_source(gofumpt, path, data):
    result = subprocess.run(
        [str(gofumpt), "-lang=go1.26", "-modpath=fdb.dev"],
        input=data,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode:
        raise ValueError(f"gofumpt failed for {path}: {result.stderr.decode('utf-8', errors='replace')}")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path, help="exact v0.42.0 module directory or module zip; never downloads")
    parser.add_argument("--gofumpt", required=True, type=Path, help="repository-pinned mvdan.cc/gofumpt v0.9.2 executable")
    parser.add_argument("--check", action="store_true", help="compare regenerated bytes without writing")
    parser.add_argument("--root", type=Path, default=ROOT, help="snapshot directory (Bazel runfiles may supply this explicitly)")
    args = parser.parse_args()
    gofumpt = args.gofumpt.resolve()
    entries = manifest(args.root / "SOURCE_SHA256SUMS")
    sources = read_sources(args.source, entries)
    outputs = {}
    for path, digest in entries.items():
        actual = hashlib.sha256(sources[path]).hexdigest()
        if actual != digest:
            raise ValueError(f"source SHA-256 mismatch for {path}: {actual}")
        outputs[path] = localize(path, sources[path])
    for path, data in outputs.items():
        if path.endswith(".go"):
            outputs[path] = format_source(gofumpt, path, data)
    # Validate every input and format every Go file before any output is written.
    mismatches = []
    for path, data in outputs.items():
        target = args.root / path
        if args.check:
            if not target.is_file() or target.read_bytes() != data:
                mismatches.append(path)
        else:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
    if mismatches:
        raise ValueError("snapshot differs: " + ", ".join(mismatches))
    action = "verified" if args.check else "wrote"
    print(f"{action} {len(outputs)} files from {MODULE}@{VERSION} ({len(PACKAGES)} runtime packages)")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, zipfile.BadZipFile) as error:
        print(error, file=sys.stderr)
        sys.exit(1)

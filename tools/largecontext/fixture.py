# /// script
# requires-python = ">=3.11"
# dependencies = ["tiktoken==0.12.0"]
# ///
"""Regenerate the public-source, exactly 500,000-token context fixture."""

import argparse
import gzip
import hashlib
import importlib.metadata
import json
from pathlib import Path
import subprocess

import tiktoken


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="4af32c3bc68698fbe26438a2f84f82ba5ec8d23f")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    revision = git(root, "rev-parse", args.revision).decode().strip()
    paths = git(root, "ls-tree", "-r", "--name-only", revision).decode().splitlines()
    code = [p for p in paths if Path(p).parent.as_posix() == "pkg/recordlayer"
            and p.endswith(".go") and not p.endswith("_test.go")]
    docs = [p for p in paths if Path(p).parent.as_posix() == "docs"
            and p.endswith(".md") and not Path(p).name.startswith(("rfc", "review", "prod-", "panic-", "test-"))]
    # Hash order avoids selecting alphabetically convenient or hand-picked code.
    order = lambda p: hashlib.sha256(p.encode()).digest()
    code.sort(key=order)
    docs.sort(key=order)
    ordered = []
    while code or docs:
        ordered.extend(code[:4])
        del code[:4]
        ordered.extend(docs[:1])
        del docs[:1]
    blocks, sources, offset = [], [], 0
    for path in ordered:
        data = git(root, "show", f"{revision}:{path}")
        if b"Code generated" in data[:500]:
            continue
        text = data.decode("utf-8")
        header = f"\n<source path={json.dumps(path)}>\n".encode()
        block = header + text.encode() + b"\n</source>\n"
        sources.append({"path": path, "sha256": hashlib.sha256(data).hexdigest(),
                        "source_bytes": len(data), "context_offset": offset,
                        "header_bytes": len(header), "block_bytes": len(block)})
        blocks.append(block)
        offset += len(block)
    encoding = tiktoken.get_encoding("cl100k_base")
    tokens = encoding.encode(b"".join(blocks).decode("utf-8"), disallowed_special=())
    if len(tokens) < 500_000:
        raise ValueError(f"insufficient distinct source: {len(tokens)} tokens")
    context = encoding.decode_bytes(tokens[:500_000]).decode("utf-8", errors="strict")
    if encoding.encode(context, disallowed_special=()) != tokens[:500_000]:
        raise ValueError("truncated token boundary did not round trip")
    context_bytes = context.encode()
    selected = []
    for source in sources:
        included = min(source["block_bytes"], len(context_bytes) - source["context_offset"])
        if included <= 0:
            break
        source["included_block_bytes"] = included
        source["included_source_bytes"] = max(0, min(source["source_bytes"], included - source["header_bytes"]))
        selected.append(source)
    messages = {"messages": [
        {"role": "system", "content": "Answer questions using the supplied source context."},
        {"role": "user", "content": context},
    ]}
    raw = (json.dumps(messages, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
    ranks = hashlib.sha256()
    for token, rank in sorted(encoding._mergeable_ranks.items()):
        ranks.update(len(token).to_bytes(4, "big"))
        ranks.update(token)
        ranks.update(rank.to_bytes(4, "big"))
    manifest = {
        "schema": 1,
        "kind": "distinct_repository_code_and_technical_docs_not_user_traffic",
        "source_revision": revision,
        "tokenizer": "tiktoken",
        "tokenizer_version": importlib.metadata.version("tiktoken"),
        "encoding": encoding.name,
        "mergeable_ranks_sha256": ranks.hexdigest(),
        "context_tokens": 500_000,
        "context_bytes": len(context_bytes),
        "context_sha256": hashlib.sha256(context_bytes).hexdigest(),
        "messages_json_tokens": len(encoding.encode(raw.decode(), disallowed_special=())),
        "messages_json_bytes": len(raw),
        "messages_json_sha256": hashlib.sha256(raw).hexdigest(),
        "sources": selected,
    }
    dest = root / "pkg/recordlayer/testdata/large_context"
    dest.mkdir(parents=True, exist_ok=True)
    (dest / "messages_500k.json.gz").write_bytes(gzip.compress(raw, compresslevel=9, mtime=0))
    (dest / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(json.dumps({k: v for k, v in manifest.items() if k != "sources"}, indent=2))
    print(f"sources={len(selected)} gzip_fixture_bytes={(dest / 'messages_500k.json.gz').stat().st_size}")


if __name__ == "__main__":
    main()

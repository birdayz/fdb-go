#!/usr/bin/env python3
"""Harvest every schema_template body containing CREATE [UNIQUE|VECTOR] INDEX from the
Java yaml-tests corpus (fdb-record-layer/, tag 4.14.2.0) into
conformance/testdata/rfc257/java_index_templates.json.

Line-based on purpose: several corpus files carry yamsql-only YAML constructs
(custom tags, duplicate keys) that a generic YAML loader rejects, and a
schema_template block is always a top-level key whose body is the indented run
of lines that follows it. A versioned template (a list of `definition:` variants
gated by initialVersionAtLeast / initialVersionLessThan) yields one entry per
variant, labelled with its gate.

A statement CENSUS reconciles the harvest with the corpus: every
`create [unique|vector] index` occurrence in every .yamsql file must be either
inside a harvested template body or on a comment line, and the comment
occurrences are recorded by file and line. The harvest refuses to write when an
occurrence is unaccounted for, so a template form this script cannot read shows
up as a failure here, not as a silently smaller corpus.

Usage: harvest.py <java-checkout> <out.json>
"""
import hashlib, json, os, re, subprocess, sys

INDEX_RE = re.compile(r'\bcreate\s+(unique\s+|vector\s+)?index\b', re.I)


def blocks(lines):
    i = 0
    while i < len(lines):
        m = re.match(r'^schema_template:\s*(.*)$', lines[i])
        if not m:
            i += 1
            continue
        head = m.group(1).strip()
        body = []
        j = i + 1
        while j < len(lines) and not lines[j].startswith('---') and (
                lines[j].startswith(' ') or lines[j].strip() == ''):
            body.append(lines[j])
            j += 1
        yield i + 1, head, body
        i = j


QUERY_RE = re.compile(r'^(\s*)-\s*query:\s*create\s+schema\s+template\s+(\S+)\s*(.*)$', re.I)


def query_templates(lines):
    """Yield (line, body, end) for every `- query: create schema template NAME ...`
    step; its continuation lines are the ones indented deeper than the dash, and
    end is the index one past the last of them."""
    for i, l in enumerate(lines):
        m = QUERY_RE.match(l)
        if not m:
            continue
        indent = len(m.group(1))
        body = [m.group(3)]
        j = i + 1
        while j < len(lines) and lines[j].strip() != '' and (
                len(lines[j]) - len(lines[j].lstrip()) > indent + 1) and not \
                lines[j].lstrip().startswith('- '):
            body.append(strip_comment(lines[j]))
            j += 1
        yield i + 1, ' '.join(' '.join(body).split()), j


def strip_comment(line):
    s = line.strip()
    return '' if s.startswith('#') else line


def variants(head, body):
    """Return [(gate, text)] for one schema_template block."""
    if not any(re.match(r'^\s*-\s*initialVersion', l) for l in body):
        text = [head] if head not in ('', '|', '>') else []
        text += [strip_comment(l) for l in body]
        return [('', ' '.join(' '.join(text).split()))]
    out = []
    gate, cur, in_def = None, [], False
    for l in body:
        g = re.match(r'^\s*-\s*(initialVersion\w+:\s*\S+)', l)
        if g:
            if gate is not None:
                out.append((gate, ' '.join(' '.join(cur).split())))
            gate, cur, in_def = g.group(1).replace(' ', ''), [], False
            continue
        d = re.match(r'^\s*definition:\s*(.*)$', l)
        if d:
            in_def = True
            if d.group(1).strip() not in ('', '|', '>'):
                cur.append(d.group(1))
            continue
        if in_def:
            cur.append(strip_comment(l))
    if gate is not None:
        out.append((gate, ' '.join(' '.join(cur).split())))
    return out


def main(root, out_path):
    res = os.path.join(root, 'yaml-tests/src/test/resources')
    tag = subprocess.run(['git', '-C', root, 'describe', '--tags', '--exact-match'],
                         capture_output=True, text=True).stdout.strip()
    sha = subprocess.run(['git', '-C', root, 'rev-parse', 'HEAD'],
                         capture_output=True, text=True).stdout.strip()
    templates = []
    occurrences, in_bodies, comments, unaccounted = 0, 0, [], []
    for dirpath, _, files in sorted(os.walk(res)):
        for f in sorted(files):
            if not f.endswith('.yamsql'):
                continue
            path = os.path.join(dirpath, f)
            raw = open(path, 'rb').read()
            rel = os.path.relpath(path, res)
            lines = raw.decode().split('\n')
            digest = hashlib.sha256(raw).hexdigest()[:16]
            covered = set()
            for line, head, body in blocks(lines):
                covered.update(range(line - 1, line + len(body)))
                for gate, text in variants(head, body):
                    if INDEX_RE.search(text):
                        templates.append({'file': rel, 'line': line, 'form': 'schema_template',
                                          'variant': gate, 'sha256': digest, 'body': text})
            for line, text, end in query_templates(lines):
                covered.update(range(line - 1, end))
                if INDEX_RE.search(text):
                    templates.append({'file': rel, 'line': line, 'form': 'query',
                                      'variant': '', 'sha256': digest, 'body': text})
            for i, l in enumerate(lines):
                for _ in INDEX_RE.finditer(l):
                    occurrences += 1
                    if l.strip().startswith('#'):
                        comments.append({'file': rel, 'line': i + 1})
                    elif i in covered:
                        in_bodies += 1
                    else:
                        unaccounted.append(f'{rel}:{i + 1}: {l.strip()[:100]}')
    n_idx = sum(len(INDEX_RE.findall(t['body'])) for t in templates)
    if unaccounted:
        sys.exit('census: occurrences outside every harvested template and comment:\n  ' +
                 '\n  '.join(unaccounted))
    # A versioned template contributes one entry per variant, so a variant's
    # statements are counted once per entry; the census counts source lines.
    variant_extra = n_idx - in_bodies
    json.dump({'source': {'tag': tag, 'commit': sha,
                          'root': 'yaml-tests/src/test/resources'},
               'census': {'occurrences': occurrences, 'in_template_bodies': in_bodies,
                          'on_comment_lines': comments, 'harvested_statements': n_idx,
                          'variant_duplicates': variant_extra},
               'templates': templates}, open(out_path, 'w'), indent=1)
    print(f'{len(templates)} templates, {n_idx} CREATE INDEX statements, '
          f'{len({t["file"] for t in templates})} files ({tag} {sha[:9]}); census '
          f'{occurrences} occurrences = {in_bodies} in bodies + {len(comments)} on comments')


if __name__ == '__main__':
    main(sys.argv[1], sys.argv[2])

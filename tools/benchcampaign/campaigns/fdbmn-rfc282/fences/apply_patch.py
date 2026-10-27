"""Exact ordinary unified-patch application; no subprocess, offsets or fuzz."""
import re


def apply_patch(sources, text):
    """Apply every hunk to exactly its declared old/new line positions."""
    lines, index, outputs = text.splitlines(keepends=True), 0, {}
    while index < len(lines):
        if not lines[index].startswith('--- a/') or index + 1 >= len(lines):
            raise ValueError('missing old file header')
        name = lines[index][6:].rstrip('\n')
        if name not in sources or name in outputs or lines[index + 1] != '+++ b/' + name + '\n':
            raise ValueError('unknown, repeated or mismatched file')
        index += 2
        original, output, position = sources[name].splitlines(keepends=True), [], 0
        hunks = 0
        while index < len(lines) and lines[index].startswith('@@ '):
            match = re.fullmatch(r'@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@\n', lines[index])
            if not match:
                raise ValueError('invalid hunk header')
            start = int(match[1]) - 1
            old_count, new_count = int(match[2] or 1), int(match[4] or 1)
            if start < position or start > len(original):
                raise ValueError('invalid old offset')
            output.extend(original[position:start])
            if len(output) != int(match[3]) - 1:
                raise ValueError('invalid new offset')
            position, removed, added = start, 0, 0
            index += 1
            while index < len(lines) and not lines[index].startswith(('@@ ', '--- a/')):
                line = lines[index]
                if line[:1] not in (' ', '+', '-'):
                    raise ValueError('invalid hunk line')
                if line[0] != '+':
                    if position >= len(original) or original[position] != line[1:]:
                        raise ValueError('source context changed')
                    position += 1
                    removed += 1
                if line[0] != '-':
                    output.append(line[1:])
                    added += 1
                index += 1
            if (removed, added) != (old_count, new_count):
                raise ValueError('hunk counts disagree')
            hunks += 1
        if not hunks:
            raise ValueError('file without patch hunks')
        output.extend(original[position:])
        outputs[name] = ''.join(output)
    if not outputs or set(outputs) != set(sources):
        raise ValueError('incomplete patch population')
    return outputs

#!/usr/bin/env python3
"""Checks the rules of docs/writing-manuals.md that a script can read. Exits 1 if a manual breaks one.

usage: check-manual-style.py FILE...

It reads prose only: code blocks, tables, headings, HTML comments and inline code are left out. Inline code counts as
one word. It does not know the STE dictionary, so passing here does not mean a manual follows the standard.
"""
import re
import sys

MAX_INSTRUCTION = 20
MAX_DESCRIPTION = 25
MAX_PARAGRAPH = 6

# Words to replace. This is a short list of common cases from this project's manuals, not the STE dictionary.
REPLACE = {
    "ensure": "make sure",
    "ensures": "makes sure",
    "utilize": "use",
    "utilise": "use",
    "leverage": "use",
    "via": "through",
    "whilst": "while",
    "in order to": "to",
    "please": "(remove)",
    "simply": "(remove)",
    "e.g.": "for example",
    "i.e.": "that is",
    "etc.": "(give the full list)",
    "and/or": "(write two sentences, or use one word)",
}
CONTRACTION = re.compile(r"\b\w+(?:n't|'ll|'re|'ve|'d)\b", re.I)
ABBREVIATED = re.compile(r"\b(e\.g|i\.e|etc|vs|cf)\.", re.I)
SENTENCE_END = re.compile(r"(?<=[.!?])\s+(?=[\"'(*`\[]*[A-Z0-9])")
ORDERED = re.compile(r"^\s*\d+\.\s+")
BULLET = re.compile(r"^\s*[-*+]\s+")


def clean(text):
    """Prose without markup: inline code becomes one word, a link becomes its text, emphasis marks go."""
    text = re.sub(r"`[^`]*`", "CODE", text)
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"<https?://[^>]*>", "LINK", text)
    text = re.sub(r"https?://\S+", "LINK", text)
    return re.sub(r"[*_]{1,3}", "", text).strip()


def blocks(lines):
    """Yields (line number, kind, text) per paragraph or list item. Kind is "step", "item" or "para"."""
    fence = comment = False
    current, start, kind = [], 0, "para"

    def flush():
        nonlocal current
        if current:
            yield start, kind, " ".join(current)
        current = []

    for n, raw in enumerate(lines, 1):
        line = raw.rstrip("\n")
        stripped = line.strip()
        if stripped.startswith(("```", "~~~")):
            yield from flush()
            fence = not fence
            continue
        if fence:
            continue
        if comment or stripped.startswith("<!--"):
            yield from flush()
            comment = "-->" not in stripped
            continue
        if not stripped or stripped.startswith(("#", "|")) or re.fullmatch(r"[-=*_ ]{3,}", stripped):
            yield from flush()
            continue
        if ORDERED.match(line) or BULLET.match(line):
            yield from flush()
            start, kind = n, "step" if ORDERED.match(line) else "item"
            current = [BULLET.sub("", ORDERED.sub("", line)).strip()]
            continue
        if not current:
            start, kind = n, "para"
        current.append(re.sub(r"^>\s*", "", stripped))
    yield from flush()


def check(path):
    with open(path, encoding="utf-8") as f:
        lines = f.readlines()
    problems = []
    for line, kind, text in blocks(lines):
        prose = clean(text)
        for word, better in REPLACE.items():
            if re.search(r"(?<![\w-])" + re.escape(word) + r"(?![\w-])", prose, re.I):
                problems.append((line, f'"{word}": use {better}'))
        for m in CONTRACTION.finditer(prose):
            problems.append((line, f'contraction "{m.group(0)}"'))
        safe = ABBREVIATED.sub(lambda m: m.group(1), prose)
        sentences = [s for s in SENTENCE_END.split(safe) if s.strip()]
        limit = MAX_INSTRUCTION if kind == "step" else MAX_DESCRIPTION
        for s in sentences:
            words = len(s.split())
            if words > limit:
                problems.append((line, f"{words} words, limit {limit}: {s[:60]}..."))
        if kind == "para" and len(sentences) > MAX_PARAGRAPH:
            problems.append((line, f"{len(sentences)} sentences in a paragraph, limit {MAX_PARAGRAPH}"))
    return problems


def main(paths):
    if not paths:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    failed = 0
    for path in paths:
        for line, message in check(path):
            print(f"{path}:{line}: {message}")
            failed += 1
    if failed:
        print(f"{failed} problems. The rules are in docs/writing-manuals.md.", file=sys.stderr)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

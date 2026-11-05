#!/usr/bin/env bash
# Renders the published documentation into a GitHub wiki checkout. The repository is the reviewed source; the wiki
# is a copy, rebuilt from scratch on every run.
# usage: wiki-render.sh OUT_DIR REPO [REF]     (REPO is owner/name, REF defaults to main)
# Pages: README.md -> Home, docs/<name>.md -> <name>, VERSIONING.md, CHANGELOG.md -> same name.
# Links to another published page become wiki links; any other relative link points back to the repository at REF.
set -euo pipefail

out=${1:?usage: wiki-render.sh OUT_DIR REPO [REF]}
repo=${2:?usage: wiki-render.sh OUT_DIR REPO [REF]}
ref=${3:-main}
blob="https://github.com/${repo}/blob/${ref}"

page() {
	case $1 in
	README.md) echo Home ;;
	*) basename "$1" .md ;;
	esac
}

sources=(README.md VERSIONING.md CHANGELOG.md)
while IFS= read -r f; do sources+=("$f"); done < <(git ls-files 'docs/*.md' | grep -v '^docs/superpowers/')

find "$out" -maxdepth 1 -name '*.md' -delete
for f in "${sources[@]}"; do
	[ -f "$f" ] || continue
	dir=$(dirname "$f")
	# Resolve each relative link against the file's directory, then map it to a page or to the repository.
	python3 - "$f" "$dir" "$blob" "${sources[@]}" >"$out/$(page "$f").md" <<'PY'
import os, re, sys
path, base, blob, *sources = sys.argv[1:]
pages = {s: ("Home" if s == "README.md" else os.path.splitext(os.path.basename(s))[0]) for s in sources}
def fix(m):
    text, target = m.group(1), m.group(2)
    if re.match(r'^([a-z]+:|#|/)', target):
        return m.group(0)
    file, _, anchor = target.partition('#')
    resolved = os.path.normpath(os.path.join(base, file)) if file else path
    if resolved in pages:
        return f'[{text}]({pages[resolved]}{"#" + anchor if anchor else ""})'
    return f'[{text}]({blob}/{resolved}{"#" + anchor if anchor else ""})'
src = open(path, encoding='utf-8').read()
sys.stdout.write(re.sub(r'\[([^\]]*)\]\(([^)\s]+)\)', fix, src))
PY
done

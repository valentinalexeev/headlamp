#!/usr/bin/env bash
# Renders the published documentation into a GitHub wiki checkout. The repository is the reviewed source; the wiki
# is a copy, rebuilt from scratch on every run.
# usage: wiki-render.sh OUT_DIR REPO [REF]     (REPO is owner/name, REF defaults to main)
# Pages:
#   README.md -> Home, VERSIONING.md, UPGRADING.md, CHANGELOG.md -> same name, docs/<name>.md -> <name>
#   <dir>/README.md (an app, role or module manual) -> <dir> with "/" replaced by "-", without a leading "apps-" or
#   "ansible-roles-" (apps/funnel-ingress/README.md -> funnel-ingress,
#   ansible/roles/task-runner/README.md -> role-task-runner, terraform/modules/exit-node-vm/oci/README.md ->
#   terraform-exit-node-vm-oci). Test fixtures are left out.
# A _Sidebar page lists every page. Links to another published page become wiki links; any other relative link points
# back to the repository at REF.
set -euo pipefail

out=${1:?usage: wiki-render.sh OUT_DIR REPO [REF]}
repo=${2:?usage: wiki-render.sh OUT_DIR REPO [REF]}
ref=${3:-main}
blob="https://github.com/${repo}/blob/${ref}"

sources=(README.md VERSIONING.md UPGRADING.md CHANGELOG.md)
while IFS= read -r f; do sources+=("$f"); done < <(
	git ls-files 'docs/*.md' | grep -v '^docs/superpowers/'
	git ls-files '*/README.md' | grep -v -E '^(docs|scripts/test)/'
)

find "$out" -maxdepth 1 -name '*.md' -delete
python3 - "$out" "$blob" "${sources[@]}" <<'PY'
import os, re, sys
out, blob, *sources = sys.argv[1:]

def page(s):
    if s == "README.md":
        return "Home"
    if os.path.basename(s) == "README.md":
        d = os.path.dirname(s)
        for prefix, repl in (("apps/", ""), ("ansible/roles/", "role-"), ("terraform/modules/", "terraform-")):
            if d.startswith(prefix):
                d = repl + d[len(prefix):]
                break
        return d.replace("/", "-")
    return os.path.splitext(os.path.basename(s))[0]

pages = {s: page(s) for s in sources}
if len(set(pages.values())) != len(pages):
    seen = {}
    for s, p in pages.items():
        if p in seen:
            sys.exit(f"two files map to the wiki page {p}: {seen[p]} and {s}")
        seen[p] = s

def render(path):
    base = os.path.dirname(path)
    def fix(m):
        text, target = m.group(1), m.group(2)
        if re.match(r'^([a-z]+:|#|/)', target):
            return m.group(0)
        file, _, anchor = target.partition('#')
        resolved = os.path.normpath(os.path.join(base, file)) if file else path
        suffix = "#" + anchor if anchor else ""
        if resolved in pages:
            return f'[{text}]({pages[resolved]}{suffix})'
        if os.path.isdir(resolved) and os.path.join(resolved, "README.md") in pages:
            return f'[{text}]({pages[os.path.join(resolved, "README.md")]}{suffix})'
        return f'[{text}]({blob}/{resolved}{suffix})'
    src = open(path, encoding='utf-8').read()
    return re.sub(r'\[([^\]]*)\]\(([^)\s]+)\)', fix, src)

for s, p in pages.items():
    with open(os.path.join(out, p + ".md"), "w", encoding="utf-8") as f:
        f.write(render(s))

# Sidebar: the project pages first, then the manuals of each part, grouped by where they live.
groups = {"Project": [], "Guides": [], "Components": [], "Ansible roles": [], "Terraform modules": [], "Other": []}
for s, p in pages.items():
    if s in ("README.md", "VERSIONING.md", "CHANGELOG.md"):
        groups["Project"].append(p)
    elif s.startswith("docs/"):
        groups["Guides"].append(p)
    elif s.startswith("apps/"):
        groups["Components"].append(p)
    elif s.startswith("ansible/roles/"):
        groups["Ansible roles"].append(p)
    elif s.startswith("terraform/"):
        groups["Terraform modules"].append(p)
    else:
        groups["Other"].append(p)
with open(os.path.join(out, "_Sidebar.md"), "w", encoding="utf-8") as f:
    for g, ps in groups.items():
        if not ps:
            continue
        f.write(f"**{g}**\n\n")
        for p in (ps if g == "Project" else sorted(ps)):
            f.write(f"- [[{p}]]\n")
        f.write("\n")
PY

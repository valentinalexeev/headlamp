#!/usr/bin/env bash
# Release preflight (#192): what a site meets on upgrade, checked before a tag.
#   1. Every Ansible role in --check mode (and syntax) against a throwaway systemd container (the OS matrix harness).
#   2. Every image in .github/components.json builds.
#   3. Every app with an overlay in examples/private-repo/fly/<app>/ builds with that overlay laid over it, and each
#      [processes] command of its fly.toml starts and keeps running. The fly.toml of the previous stable tag is
#      started too: a process command that a site already has must still work, or the release must say so
#      (ALLOW_PROCESS_CHANGE=1 after an UPGRADING entry).
#
# Usage: scripts/release-preflight.sh [REF]      (default HEAD; a tag such as v0.12.0 checks that release tree)
# Env:   STEPS="roles images overlay" (default all), OS=debian13 (OS matrix image), SKIP_IMAGES="img ..." to skip,
#        KEEP_IMAGES=1 keeps the built images, ALLOW_PROCESS_CHANGE=1, PREFLIGHT_APPLY="tailscale" (roles applied for real).
# Needs docker (linux/amd64 emulation on arm64), git, jq, python3 (tomllib), ansible-playbook with community.docker.
# Prints "PREFLIGHT <step> <item> ok|FAIL" lines and exits 1 on any failure.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
ref="${1:-HEAD}"
steps="${STEPS:-roles images overlay}"
os="${OS:-debian13}"
for c in docker git jq python3; do command -v "$c" >/dev/null || { echo "FATAL: $c is not installed"; exit 2; }; done
sha="$(git -C "$repo" rev-parse --verify -q "$ref^{commit}")" || { echo "FATAL: unknown ref $ref"; exit 2; }
work="$(mktemp -d "${TMPDIR:-/tmp}/hl-preflight.XXXXXX")"
built=()
cleanup() {
  rm -rf "$work"
  [ "${KEEP_IMAGES:-}" = 1 ] || for i in ${built[@]+"${built[@]}"}; do docker rmi -f "$i" >/dev/null 2>&1; done
}
trap cleanup EXIT
fails=0
res() { echo "PREFLIGHT $1 $2 $3"; [ "$3" = ok ] || [ "$3" = skip ] || fails=$((fails + 1)); }

# The release tree, as a plain export (no working-tree changes leak in).
tree="$work/tree"; mkdir -p "$tree"
git -C "$repo" archive "$sha" | tar -x -C "$tree"
# The harness comes from this checkout, so an old tag is checked with today's checks.
mkdir -p "$tree/scripts/test" "$tree/ansible/tests"
rm -rf "$tree/scripts/test/ansible-os" "$tree/ansible/tests/os-matrix"
cp -R "$repo/scripts/test/ansible-os" "$tree/scripts/test/"
cp -R "$repo/ansible/tests/os-matrix" "$tree/ansible/tests/"
# The harness's headscale is a test fixture with today's config format, so it uses today's pin. The roles, the
# tailscale pin and the apps under test stay those of the release tree.
cp "$repo/HEADSCALE_VERSION" "$tree/HEADSCALE_VERSION"
for ov in "$repo"/examples/private-repo/fly/*/; do # an older tree without an example overlay gets today's
  [ -e "$tree/examples/private-repo/fly/$(basename "$ov")" ] || { mkdir -p "$tree/examples/private-repo/fly"; cp -R "${ov%/}" "$tree/examples/private-repo/fly/"; }
done
echo "release preflight of $ref ($sha)"

has() { case " $steps " in *" $1 "*) return 0 ;; esac; return 1; }

if has roles; then
  # The base tailscale role goes first: the other roles check for its client in their preflight.
  roles=(); [ -d "$tree/ansible/roles/tailscale" ] && roles=(tailscale)
  for d in "$tree"/ansible/roles/*/; do r="$(basename "$d")"; [ "$r" = tailscale ] || roles+=("$r"); done
  for r in "${roles[@]}"; do
    printf -- '- hosts: all\n  roles: [%s]\n' "$r" > "$work/syntax-$r.yml"
    if ANSIBLE_ROLES_PATH="$tree/ansible/roles" ansible-playbook -i localhost, --syntax-check "$work/syntax-$r.yml" >"$work/syntax-$r.log" 2>&1; then
      res syntax "$r" ok
    else res syntax "$r" FAIL; tail -n 15 "$work/syntax-$r.log"; fi
  done
  # Check mode for every role: the preflight asserts run. Only the base tailscale role is also applied (in the
  # throwaway container), since the other roles check for its client and address. PREFLIGHT_APPLY changes the list.
  APPLY="${PREFLIGHT_APPLY:-tailscale}" KEEP_IMAGE=1 bash "$tree/scripts/test/ansible-os/run.sh" "$os" "${roles[@]}" 2>&1 | tee "$work/roles.log" | grep -vE '^(all roles passed|[0-9]+ role\(s\) failed)'
  for r in "${roles[@]}"; do
    line="$(grep -E "^RESULT $os $r " "$work/roles.log")"
    case "$line" in *check=ok*) res check "$r" ok ;; *) res check "$r" FAIL ;; esac
  done
fi

# build <tag> <context> <dockerfile> <build-args (newline list)>
build() {
  local tag="$1" ctx="$2" df="$3" args="$4" a=()
  while IFS= read -r kv; do [ -n "$kv" ] && a+=(--build-arg "$kv"); done <<<"$args"
  docker build -q -t "$tag" -f "$ctx/$df" ${a[@]+"${a[@]}"} "$ctx" >"$work/build.log" 2>&1 && built+=("$tag")
}
args_of() { # components.json entry -> build args, values from files read from the release tree
  jq -r '(.args // {}) | to_entries[] | "\(.key)=\(.value)"' <<<"$1"
  jq -r '(.args_from_files // {}) | to_entries[] | "\(.key) \(.value)"' <<<"$1" | while read -r k f; do
    echo "$k=$(tr -d '[:space:]' < "$tree/$f")"; done
}
manifest="$tree/.github/components.json"

if has images; then
  while IFS= read -r e; do
    img="$(jq -r .image <<<"$e")"
    case " ${SKIP_IMAGES:-} " in *" $img "*) res image "$img" skip; continue ;; esac
    if build "hlpf-$img:$sha" "$tree/$(jq -r .context <<<"$e")" "$(jq -r .dockerfile <<<"$e")" "$(args_of "$e")"; then
      res image "$img" ok
    else res image "$img" FAIL; tail -n 15 "$work/build.log"; fi
  done < <(jq -c '.[]' "$manifest")
fi

# processes <fly.toml> -> one "<group>\t<command>" line for each [processes] entry
processes() { python3 -c 'import sys,tomllib
for k,v in tomllib.load(open(sys.argv[1],"rb")).get("processes",{}).items(): print(f"{k}\t{v}")' "$1"; }
# starts <image> <command>: the container runs the command (split like Fly does) and is still up after a few seconds
starts() {
  local id ok=1; read -r -a cmd <<<"$2"
  id="$(docker run -d "$1" "${cmd[@]}" 2>"$work/run.log")" || return 1
  sleep "${START_WAIT:-8}"
  [ "$(docker inspect -f '{{.State.Running}}' "$id")" = true ] || { ok=0; docker logs "$id" >"$work/run.log" 2>&1; }
  docker rm -f "$id" >/dev/null 2>&1; [ $ok = 1 ]
}

if has overlay; then
  prev="$(git -C "$repo" tag -l 'v[0-9]*.[0-9]*.[0-9]*' --merged "$sha" | grep -vE -- '-' | sort -V | grep -vx "$ref" | tail -n 1)"
  for ov in "$tree"/examples/private-repo/fly/*/; do
    app="$(basename "$ov")"; [ -d "$tree/apps/$app" ] || continue
    e="$(jq -c --arg c "apps/$app" '[.[] | select(.context == $c and .dockerfile == "Dockerfile")][0] // empty' "$manifest")"
    [ -n "$e" ] || { res overlay "$app" skip; continue; }
    ctx="$work/overlay-$app"; cp -R "$tree/apps/$app" "$ctx"; cp -R "$ov". "$ctx"/
    tag="hlpf-$app-overlay:$sha"
    if ! build "$tag" "$ctx" Dockerfile "$(args_of "$e")"; then res overlay "$app" FAIL; tail -n 15 "$work/build.log"; continue; fi
    res overlay "$app" ok
    [ -f "$ctx/fly.toml" ] || continue
    while IFS=$'\t' read -r g c; do
      if starts "$tag" "$c"; then res process "$app:$g=\"$c\"" ok
      else res process "$app:$g=\"$c\"" FAIL; tail -n 5 "$work/run.log"; fi
    done < <(processes "$ctx/fly.toml")
    # The process commands a site already runs, from the fly.toml of the previous release.
    if [ -n "$prev" ] && git -C "$repo" cat-file -e "$prev:apps/$app/fly.toml" 2>/dev/null; then
      git -C "$repo" show "$prev:apps/$app/fly.toml" > "$work/prev.toml"
      while IFS=$'\t' read -r g c; do
        if starts "$tag" "$c"; then res process "$app:$g=\"$c\"(from-$prev)" ok
        elif [ "${ALLOW_PROCESS_CHANGE:-}" = 1 ]; then res process "$app:$g=\"$c\"(from-$prev)" skip
        else
          res process "$app:$g=\"$c\"(from-$prev)" FAIL; tail -n 5 "$work/run.log"
          echo "  a site overlay with the $prev process command breaks: document it, then set ALLOW_PROCESS_CHANGE=1"
        fi
      done < <(processes "$work/prev.toml")
    fi
  done
fi

if [ $fails = 0 ]; then echo "release preflight of $ref: all checks passed"; else echo "release preflight of $ref: $fails check(s) failed"; exit 1; fi

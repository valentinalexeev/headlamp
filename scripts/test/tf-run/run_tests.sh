#!/usr/bin/env bash
# Tests for scripts/tf-run.sh and .githooks/pre-commit, using stub `sops` and `terraform`
# shellcheck disable=SC2164  # test scaffolding: cd failures surface as test failures anyway
# (bin/) in throwaway git repos -- no cloud, no real keys. Run: make test-tf-run
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; SRC="$(cd "$HERE/../../.." && pwd)"
SP="$(mktemp -d)"; trap 'rm -rf "$SP"' EXIT
export PATH="$HERE/bin:$PATH" SOPS_AGE_KEY=stub GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
pass=0; failn=0
ok()   { echo "  PASS  $1"; pass=$((pass+1)); }
no()   { echo "  FAIL  $1"; failn=$((failn+1)); }
t()    { if eval "$2"; then ok "$1"; else no "$1"; fi; }

newrepo() { # newrepo <name> [remote]
  R="$SP/$1"; rm -rf "$R" "$SP/$1.git"; mkdir -p "$R/terraform" "$R/scripts"; cd "$R"
  git init -q -b main; git config core.hooksPath "$SRC/.githooks"
  cp "$SRC/scripts/tf-run.sh" scripts/; cp "$SRC/.gitattributes" .
  echo 'creation_rules:
  - age: age1testrecipient1,age1backuprecipient2' > .sops.yaml
  echo '{"serial": 1, "resources": ["r1"]}' > terraform/terraform.tfstate
  sops -e --config /dev/null --age x -i terraform/terraform.tfstate; mv terraform/terraform.tfstate terraform/terraform.tfstate.enc.json
  echo 'resource "x" "y" {}' > terraform/main.tf
  git add -A; git commit -q -m init
  if [ "${2:-}" = remote ]; then git init -q --bare -b main "$SP/$1.git"; git remote add origin "$SP/$1.git"; git push -q -u origin main; fi
}
serial_enc() { sops -d terraform/terraform.tfstate.enc.json | python3 -c 'import sys,json;print(json.load(sys.stdin)["serial"])'; }
no_plaintext() { [ ! -e terraform/terraform.tfstate ] && [ ! -e terraform/terraform.tfstate.backup ] && [ ! -e terraform/secrets.auto.tfvars.json ]; }

echo "== 1 plan is a no-op for the encrypted file"
newrepo t1; before=$(git rev-parse HEAD); sum=$(shasum terraform/terraform.tfstate.enc.json)
./scripts/tf-run.sh plan >/dev/null 2>&1; rc=$?
t "plan exits 0" "[ $rc = 0 ]"; t "no new commit" "[ $(git rev-parse HEAD) = $before ]"
t "enc file byte-identical" "[ \"\$(shasum terraform/terraform.tfstate.enc.json)\" = \"$sum\" ]"; t "no plaintext left" no_plaintext

echo "== 2 successful apply re-encrypts and commits"
newrepo t2; ./scripts/tf-run.sh apply >/dev/null 2>&1; rc=$?
t "apply exits 0" "[ $rc = 0 ]"; t "serial 2 in encrypted state" "[ \$(serial_enc) = 2 ]"
t "state committed" "git log -1 --format=%s | grep -q 'state after apply (serial 2)'"; t "tree clean" "[ -z \"\$(git status --porcelain)\" ]"; t "no plaintext left" no_plaintext

echo "== 3 failed (partial) apply still saves state and propagates rc"
newrepo t3; STUB_APPLY_RC=1 ./scripts/tf-run.sh apply >/dev/null 2>&1; rc=$?
t "exit code propagated (1)" "[ $rc = 1 ]"; t "state saved despite failure" "[ \$(serial_enc) = 2 ]"
t "commit flags partial apply" "git log -1 --format=%s | grep -q 'possibly partial'"; t "no plaintext left" no_plaintext

echo "== 4 dirty terraform/ blocks apply; ALLOW_DIRTY overrides; plan only warns"
newrepo t4; echo '# edit' >> terraform/main.tf
./scripts/tf-run.sh apply >/dev/null 2>&1; rc=$?; t "apply refused when dirty" "[ $rc != 0 ] && [ \$(serial_enc) = 1 ]"
./scripts/tf-run.sh plan >/dev/null 2>&1; t "plan allowed when dirty" "[ $? = 0 ]"
ALLOW_DIRTY=1 ./scripts/tf-run.sh apply >/dev/null 2>&1; t "ALLOW_DIRTY=1 applies" "[ $? = 0 ] && [ \$(serial_enc) = 2 ]"

echo "== 5 leftover plaintext state blocks a run; save recovers it"
newrepo t5; echo '{"serial": 7, "resources": ["unsaved"]}' > terraform/terraform.tfstate
./scripts/tf-run.sh plan >/dev/null 2>&1; t "plan refused with leftover plaintext" "[ $? != 0 ] && [ -e terraform/terraform.tfstate ]"
./scripts/tf-run.sh save >/dev/null 2>&1; t "save succeeds" "[ $? = 0 ] && [ \$(serial_enc) = 7 ]"; t "plaintext removed after save" no_plaintext

echo "== 6 behind upstream blocks apply"
newrepo t6 remote; git clone -q "$SP/t6.git" "$SP/t6b" 2>/dev/null; ( cd "$SP/t6b" && echo x > f && git add f && git commit -q -m other && git push -q )
./scripts/tf-run.sh apply >/dev/null 2>&1; rc=$?; t "apply refused when behind" "[ $rc != 0 ] && [ \$(serial_enc) = 1 ]"
ALLOW_STALE=1 ./scripts/tf-run.sh apply >/dev/null 2>&1; t "ALLOW_STALE=1 applies" "[ $? = 0 ]"
rm -rf "$SP/t6b"

echo "== 7 with an upstream, the state commit is pushed"
newrepo t7 remote; ./scripts/tf-run.sh apply >/dev/null 2>&1
t "remote has the state commit" "[ \"\$(git rev-parse HEAD)\" = \"\$(git --git-dir=$SP/t7.git rev-parse main)\" ]"

echo "== 8 encrypt failure keeps the plaintext state (never loses it)"
newrepo t8; STUB_SOPS_FAIL_ENC=1 ./scripts/tf-run.sh apply >/dev/null 2>&1; rc=$?
t "run failed" "[ $rc != 0 ]"; t "plaintext state preserved" "[ -s terraform/terraform.tfstate ] && grep -q r2 terraform/terraform.tfstate"
t "encrypted state untouched" "[ \$(serial_enc) = 1 ]"; t "secrets plaintext not kept" "[ ! -e terraform/secrets.auto.tfvars.json ]"
./scripts/tf-run.sh save >/dev/null 2>&1; t "save recovers after fixing the cause" "[ $? = 0 ] && [ \$(serial_enc) = 2 ]"

echo "== 9 SIGINT during apply: terraform stops, state is still saved"
newrepo t9
python3 - "$SP" <<'PY'
import subprocess, signal, time, os, sys
sp=sys.argv[1]
p=subprocess.Popen(["./scripts/tf-run.sh","apply"],cwd=os.path.join(sp,"t9"),start_new_session=True,env=dict(os.environ,STUB_APPLY_SLEEP="30"),stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
time.sleep(3); os.killpg(p.pid, signal.SIGINT); rc=p.wait(timeout=30); print("rc=%d"%rc)
PY
t "state saved after Ctrl-C" "[ \$(serial_enc) = 2 ]"; t "no plaintext left" no_plaintext

echo "== 10 pre-commit hook"
newrepo t10; cd "$SP/t10"
echo '{"serial": 9}' > terraform/terraform.tfstate; git add -f terraform/terraform.tfstate
git commit -q -m x >/dev/null 2>&1; t "plaintext tfstate rejected" "[ $? != 0 ]"; git reset -q; rm -f terraform/terraform.tfstate
echo '{"not":"encrypted"}' > terraform/bad.enc.json; git add -f terraform/bad.enc.json
git commit -q -m x >/dev/null 2>&1; t "unencrypted *.enc.json rejected" "[ $? != 0 ]"; git reset -q; rm -f terraform/bad.enc.json
printf 'k=%s\n' "AGE-SECRET-KEY-1$(printf 'Q%.0s' $(seq 1 55))" > leak.txt; git add leak.txt
git commit -q -m x >/dev/null 2>&1; t "age private key rejected" "[ $? != 0 ]"; git reset -q; rm -f leak.txt
echo hello > ok.txt; git add ok.txt; git commit -q -m ok >/dev/null 2>&1; t "ordinary file commits" "[ $? = 0 ]"
cp terraform/terraform.tfstate.enc.json terraform/x.enc.json; git add terraform/x.enc.json; git commit -q -m enc >/dev/null 2>&1; t "properly encrypted *.enc.json commits" "[ $? = 0 ]"

echo; echo "passed=$pass failed=$failn"; [ "$failn" = 0 ]

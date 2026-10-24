#!/bin/sh
# Tests for probe.sh (process selection) and the argument checks of both scripts. No fly needed.
set -u
dir=$(cd "$(dirname "$0")" && pwd)
t=$(mktemp -d) || exit 2
trap 'rm -rf "$t"' EXIT
fail=0
check() { # check <name> <test args...>
  name=$1; shift
  if [ "$@" ]; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}

BINP=/usr/local/bin/node-control
mkproc() { # mkproc <root> <pid> <exe> <comm> <cmdline> <rss>
  mkdir -p "$1/$2"
  ln -s "$3" "$1/$2/exe"
  echo "$4" >"$1/$2/comm"
  printf '%s' "$5" >"$1/$2/cmdline"
  printf 'Name:\t%s\nVmRSS:\t%s kB\n' "$4" "$6" >"$1/$2/status"
}
mkmem() { printf 'MemTotal:       250000 kB\nMemAvailable:   120000 kB\n' >"$1/meminfo"; }

# 1. a decoy whose command line mentions node-control and has a LOWER pid and a tiny RSS
r=$t/one; mkdir -p "$r"; mkmem "$r"
mkproc "$r" 7 /bin/busybox sh "sh -c case */node-control* in" 900
mkproc "$r" 12 "$BINP" node-control "$BINP" 150000
mkproc "$r" 100 /bin/busybox sleep "sh -c sleep 60 # /usr/local/bin/node-control" 400
out=$(PROC_ROOT=$r BIN=$BINP sh "$dir/probe.sh" 2>&1); rc=$?
check "decoy ignored, real process picked" "$rc$out" = "012 node-control 150000 120000 250000"

# 2. only decoys: error, never a guess
r=$t/none; mkdir -p "$r"; mkmem "$r"
mkproc "$r" 7 /bin/busybox sh "sh -c node-control" 900
out=$(PROC_ROOT=$r BIN=$BINP sh "$dir/probe.sh" 2>&1); rc=$?
check "no match fails" "$rc" -ne 0

# 3. two real processes: error
r=$t/two; mkdir -p "$r"; mkmem "$r"
mkproc "$r" 12 "$BINP" node-control x 150000
mkproc "$r" 13 "$BINP" node-control x 150000
out=$(PROC_ROOT=$r BIN=$BINP sh "$dir/probe.sh" 2>&1); rc=$?
check "two matches fail" "$rc" -ne 0

# 4. the old cmdline-text lookup picks the decoy in layout 1 (documents the bug being fixed)
oldlookup() {
  for p in "$t"/one/[0-9]*; do
    case "$(tr '\0' ' ' <"$p/cmdline" 2>/dev/null)" in
      */node-control*) echo "${p##*/}"; return ;;
    esac
  done
}
old=$(oldlookup)
check "old lookup would have chosen the decoy (pid $old)" "$old" = 100

# 5. arguments starting with - are rejected
sh "$dir/measure-memory.sh" --help >/dev/null 2>&1; rc=$?
check "measure-memory rejects -app" "$rc" -eq 2
sh "$dir/measure-memory.sh" app 5 -oBad >/dev/null 2>&1; rc=$?
check "measure-memory rejects -url" "$rc" -eq 2
sh "$dir/parity.sh" -x http://127.0.0.1 >/dev/null 2>&1; rc=$?
check "parity rejects -arg" "$rc" -eq 2
sh "$dir/parity.sh" http://127.0.0.1 -x >/dev/null 2>&1; rc=$?
check "parity rejects second -arg" "$rc" -eq 2

# 6. end to end with a fake fly that runs the probe against the fake /proc
mkdir -p "$t/bin"
cat >"$t/bin/fly" <<FAKE
#!/bin/sh
PROC_ROOT=$t/one BIN=$BINP exec sh -c "\$6"
FAKE
chmod +x "$t/bin/fly"
out=$(PATH=$t/bin:$PATH sh "$dir/measure-memory.sh" app 1 2>&1); rc=$?
echo "$out" | grep -q 'pid 12 (node-control)'; seen=$?
check "end to end PASS prints pid and comm" "$seen$rc" = 00
exit "$fail"

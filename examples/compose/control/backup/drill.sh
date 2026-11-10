#!/bin/sh
# Disaster recovery drill with no downtime. Runs restore.sh into a temporary directory, compares the result with
# the live data, and prints the time. It writes nothing to the live data. It prints counts and hashes only.
# Run it from the compose directory.
set -eu
. ./.env
D=${DATA_DIR:-/srv/headlamp/data}
S=$(mktemp -d /tmp/drill.XXXXXX); trap 'rm -rf "$S"' EXIT
start=$(date +%s)
RESTORE_DIR=$S sh backup/restore.sh
echo "drill: restore took $(( $(date +%s) - start )) s"

python3 - "$D" "$S" <<'PY'
import hashlib, os, sqlite3, sys
live, rest = sys.argv[1], sys.argv[2]
def db(p): return sqlite3.connect(f"file:{p}?mode=ro", uri=True)
ok = True
for name, tables in [("headscale/db.sqlite", ["users", "nodes", "pre_auth_keys", "api_keys"]),
                     ("node-control/policy.db", None), ("node-control/enroll.db", None)]:
    if not os.path.exists(f"{live}/{name}"):
        continue  # not used on this host
    if not os.path.exists(f"{rest}/{name}"):
        ok = False; print("drill:", name, "NOT RESTORED"); continue
    L, R = db(f"{live}/{name}"), db(f"{rest}/{name}")
    print("drill:", name, "integrity", R.execute("pragma integrity_check").fetchone()[0])
    if tables is None:
        tables = [r[0] for r in L.execute("select name from sqlite_master where type='table' and name not like 'sqlite_%'")]
    for t in tables:
        a, b = (c.execute(f'select count(*) from "{t}"').fetchone()[0] for c in (L, R))
        ok &= a == b
        print(f"drill:   {t} live={a} restored={b}")
for k in ["noise_private.key", "derp_server_private.key"]:
    a, b = (hashlib.sha256(open(f"{d}/headscale/{k}", "rb").read()).hexdigest() for d in (live, rest))
    ok &= a == b
    print(f"drill: {k} {'same' if a == b else 'DIFFERENT'}")
print("drill: databases and keys", "MATCH" if ok else "DIFFER")
PY

if [ -d "$S/redis" ] && [ -n "${REDIS_IMAGE:-}" ]; then
  # Change the redis-cli options to match the authentication of your Redis service.
  live_keys=$(docker compose exec -T redis sh -c 'redis-cli -a "$(cat /run/secrets/redis_password)" --no-auth-warning dbsize')
  docker run -d --name drill-redis --network none -v "$S/redis:/data" "$REDIS_IMAGE" redis-server --appendonly yes --dir /data >/dev/null
  i=0; until docker exec drill-redis redis-cli ping 2>/dev/null | grep -q PONG || [ $i -ge 30 ]; do i=$((i+1)); sleep 1; done
  rest_keys=$(docker exec drill-redis redis-cli dbsize)
  docker rm -f drill-redis >/dev/null
  echo "drill: redis keys live=$live_keys restored=$rest_keys"
fi
echo "drill: total $(( $(date +%s) - start )) s"

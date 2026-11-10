#!/bin/sh
# Puts the control-host data back on a NEW host, before the first `docker compose up`. Run it from the compose
# directory. It does nothing when headscale/db.sqlite exists: it never writes over a live database.
#   headscale/db.sqlite, node-control/policy.db, node-control/enroll.db   from Litestream
#   headscale keys, Redis data                                           from the copies of backup.sh
# RESTORE_DIR=<directory> restores into another directory (drill.sh uses this on a live host).
set -eu
. ./.env
: "${BACKUP_BUCKET:?}" "${BACKUP_REGION:?}" "${AWS_CLI_IMAGE:?}" "${LITESTREAM_IMAGE:?}"
D=${RESTORE_DIR:-${DATA_DIR:-/srv/headlamp/data}}
mkdir -p "$D/headscale" "$D/node-control"
if [ -f "$D/headscale/db.sqlite" ]; then
  echo "restore: $D/headscale/db.sqlite exists: nothing to do"; exit 0
fi
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT; chmod 755 "$T"

s3get() { # <object under backup/> <file in $T>
  docker run --rm --network host -e AWS_REGION="$BACKUP_REGION" -v "$T:/b" "$AWS_CLI_IMAGE" \
    s3 cp --only-show-errors "s3://$BACKUP_BUCKET/backup/$1" "/b/$2"
}
ls_restore() { # <path in litestream.yml> <output file>
  docker run --rm --network host -e BACKUP_BUCKET="$BACKUP_BUCKET" -e BACKUP_REGION="$BACKUP_REGION" \
    -v "$PWD/backup/litestream.yml:/etc/litestream.yml:ro" -v "$(dirname "$2"):/out" "$LITESTREAM_IMAGE" \
    restore -config /etc/litestream.yml -if-replica-exists -o "/out/$(basename "$2")" "$1"
}

ls_restore /data/headscale/db.sqlite "$D/headscale/db.sqlite"
[ -f "$D/headscale/db.sqlite" ] || { echo "restore: no headscale replica: refusing a silent fresh start"; exit 1; }
for f in policy.db enroll.db; do
  ls_restore "/data/node-control/$f" "$D/node-control/$f" || true
  [ -f "$D/node-control/$f" ] || echo "restore: NOTE no replica of node-control/$f (not used, or empty)"
done

if s3get headscale/keys.tar keys.tar; then
  tar -C "$D/headscale" -xf "$T/keys.tar"; chmod 600 "$D/headscale"/*.key
else
  echo "restore: WARNING no headscale keys copy: every client must log in again"
fi

# Redis with appendonly=yes does not read dump.rdb when it has no AOF. So start it once without AOF to load the
# snapshot, then turn AOF on (Redis writes the AOF from memory) and stop it cleanly.
if [ -n "${REDIS_IMAGE:-}" ] && s3get redis/dump.rdb dump.rdb; then
  mkdir -p "$D/redis"
  if [ -z "$(ls -A "$D/redis")" ]; then
    cp "$T/dump.rdb" "$D/redis/dump.rdb"
    docker run -d --name restore-redis --network none -v "$D/redis:/data" "$REDIS_IMAGE" \
      redis-server --appendonly no --dir /data >/dev/null
    i=0; until docker exec restore-redis redis-cli ping 2>/dev/null | grep -q PONG || [ $i -ge 30 ]; do i=$((i+1)); sleep 1; done
    docker exec restore-redis redis-cli config set appendonly yes >/dev/null
    i=0; until { docker exec restore-redis redis-cli info persistence | grep -q 'aof_rewrite_in_progress:0' \
      && [ -d "$D/redis/appendonlydir" ]; } || [ $i -ge 30 ]; do i=$((i+1)); sleep 1; done
    echo "restore: redis keys $(docker exec restore-redis redis-cli dbsize)"
    docker exec restore-redis redis-cli shutdown >/dev/null 2>&1 || true
    docker rm -f restore-redis >/dev/null 2>&1 || true
  fi
fi
echo "restore: done into $D"

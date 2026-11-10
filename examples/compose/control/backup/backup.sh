#!/bin/sh
# Nightly copy of the control-host data that Litestream does not replicate, to s3://$BACKUP_BUCKET/backup/.
# Run it from the compose directory, by hand or from a systemd timer (see README.md):
#   headscale/keys.tar         noise and DERP private keys: without them, every client must log in again
#   node-control/enroll.json   node-control enrolment export (devices, UUIDs, approvals); ENROLL_REDIS=1 only
#   redis/dump.rdb             Redis snapshot; only when the compose has a "redis" service
# The object names do not change. Turn on versioning on the bucket to keep older copies.
# The script prints sizes only, never values.
set -eu
. ./.env
: "${BACKUP_BUCKET:?set BACKUP_BUCKET in .env}" "${BACKUP_REGION:?set BACKUP_REGION in .env}"
: "${AWS_CLI_IMAGE:?pin amazon/aws-cli:<version>@sha256:<digest> in .env}"
umask 077
D=${DATA_DIR:-/srv/headlamp/data}
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT

s3put() { # <file in $T> <object under backup/>
  docker run --rm --network host -e AWS_REGION="$BACKUP_REGION" -v "$T:/b:ro" "$AWS_CLI_IMAGE" \
    s3 cp --only-show-errors "/b/$1" "s3://$BACKUP_BUCKET/backup/$2"
}

tar -C "$D/headscale" -cf "$T/keys.tar" noise_private.key derp_server_private.key
s3put keys.tar headscale/keys.tar
out="keys $(wc -c <"$T/keys.tar")B"

if grep -q '^ENROLL_REDIS=1' node-control.env 2>/dev/null; then
  docker compose exec -T node-control node-control enroll-export -format json > "$T/enroll.json"
  s3put enroll.json node-control/enroll.json
  out="$out enroll $(wc -c <"$T/enroll.json")B"
fi

if docker compose config --services | grep -qx redis; then
  # Change the redis-cli options to match the authentication of your Redis service.
  docker compose exec -T redis sh -c 'redis-cli -a "$(cat /run/secrets/redis_password)" --no-auth-warning --rdb /data/backup.rdb >/dev/null'
  mv "$D/redis/backup.rdb" "$T/dump.rdb"
  s3put dump.rdb redis/dump.rdb
  out="$out redis $(wc -c <"$T/dump.rdb")B"
fi
echo "backup: $(date -u +%FT%TZ) $out"

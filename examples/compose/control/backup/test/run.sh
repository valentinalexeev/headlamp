#!/bin/sh
# End-to-end test of examples/compose/control/backup against a local S3-compatible server (SeaweedFS), with real
# headscale data. Needs Docker on Linux (host networking, and SQLite WAL on bind mounts). On macOS, run it in dind:
#   docker run -d --privileged --name hlbk -v "$PWD/examples/compose/control/backup:/src:ro" docker:27-dind
#   docker exec hlbk sh -c 'apk add -q curl python3 && cp -r /src /work && sh /work/test/run.sh'
#   docker rm -f -v hlbk    # -v: the dind image keeps about 750 MB in an anonymous volume otherwise
# On Linux:
#   sh examples/compose/control/backup/test/run.sh
# It makes a throwaway compose directory, a headscale database with users and nodes, replicates it with Litestream,
# runs backup.sh, restores into an empty directory, runs drill.sh, and checks the output.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d); N=hlbk$$
cleanup() { docker rm -f -v "$N-s3" "$N-hs" "$N-ls" >/dev/null 2>&1 || true; docker network rm "$N" >/dev/null 2>&1 || true; rm -rf "$W"; }
trap cleanup EXIT
HS=ghcr.io/juanfont/headscale:v0.29.7
LS=litestream/litestream:0.5.2
CLI=amazon/aws-cli:2.27.0
S3D=chrislusf/seaweedfs:3.97

docker network create "$N" >/dev/null
# SeaweedFS as a small S3-compatible server (any S3 endpoint works; the test uses no cloud account).
# Host network, like the scripts' own containers (they use --network host for the instance role).
# SeaweedFS uses its default internal ports (9333, 8080, 8888 and +10000 for gRPC); the S3 port is free to pick.
port=$((18000 + $$ % 1000))
docker run -d --name "$N-s3" --network host "$S3D" server -s3 -s3.port="$port" -ip=127.0.0.1 -ip.bind=127.0.0.1 >/dev/null
EP="http://127.0.0.1:$port"
i=0; until [ "$(docker run --rm --network host curlimages/curl -s -m 3 -o /dev/null -w '%{http_code}' "$EP/")" != 000 ] || [ $i -ge 60 ]; do i=$((i+1)); sleep 2; done
sleep 5

mkdir -p "$W/data/headscale" "$W/data/node-control" "$W/backup" "$W/config"
cp "$here"/*.sh "$here/litestream.yml" "$W/backup/"
# Point Litestream and the AWS CLI at the test server: an endpoint line in the replicas, and a wrapper image env.
sed -i.bak "s|region: \${BACKUP_REGION}|region: \${BACKUP_REGION}\n        endpoint: $EP\n        force-path-style: true|" "$W/backup/litestream.yml"
cat > "$W/.env" <<EOF
DATA_DIR=$W/data
BACKUP_BUCKET=headlamp-backup-test
BACKUP_REGION=us-east-1
LITESTREAM_IMAGE=$LS
AWS_CLI_IMAGE=$CLI
EOF
aws_t() { docker run --rm --network host -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=testtest1 -e AWS_REGION=us-east-1 \
  "$CLI" --endpoint-url "$EP" "$@"; }
aws_t s3 mb s3://headlamp-backup-test >/dev/null
# The server answers HTTP before its volume server takes writes: wait for one real write.
i=0; until echo ok | aws_t s3 cp - s3://headlamp-backup-test/ready >/dev/null 2>&1 || [ $i -ge 60 ]; do i=$((i+1)); sleep 2; done

# headscale with a few users and nodes, so the drill has rows to compare.
cat > "$W/config/headscale.yaml" <<'EOF'
server_url: http://127.0.0.1:8080
listen_addr: 0.0.0.0:8080
noise: { private_key_path: /var/lib/headscale/noise_private.key }
prefixes: { v4: 100.64.0.0/10, v6: fd7a:115c:a1e0::/48, allocation: sequential }
derp: { server: { enabled: true, region_id: 999, region_code: t, region_name: t, stun_listen_addr: "0.0.0.0:3478", private_key_path: /var/lib/headscale/derp_server_private.key, automatically_add_embedded_derp_region: true }, urls: [] }
database: { type: sqlite, sqlite: { path: /var/lib/headscale/db.sqlite, write_ahead_log: true } }
dns: { magic_dns: false, base_domain: t.invalid, override_local_dns: false }
unix_socket: /var/run/headscale/headscale.sock
EOF
docker run -d --name "$N-hs" -v "$W/config/headscale.yaml:/etc/headscale/config.yaml:ro" -v "$W/data/headscale:/var/lib/headscale" \
  "$HS" serve >/dev/null
i=0; until docker exec "$N-hs" headscale users list >/dev/null 2>&1 || [ $i -ge 30 ]; do i=$((i+1)); sleep 1; done
for u in alice bob; do docker exec "$N-hs" headscale users create "$u" >/dev/null; done
for u in 1 2; do docker exec "$N-hs" headscale preauthkeys create -u "$u" -e 1h >/dev/null; done
docker exec "$N-hs" headscale apikeys create -e 1h >/dev/null
sudo_free_chmod() { docker run --rm -v "$W/data:/d" alpine chmod -R a+rwX /d; }
sudo_free_chmod

# Litestream in the background, as compose.backup.yaml runs it.
docker run -d --name "$N-ls" --network host -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=testtest1 \
  -e BACKUP_BUCKET=headlamp-backup-test -e BACKUP_REGION=us-east-1 -v "$W/backup/litestream.yml:/etc/litestream.yml:ro" \
  -v "$W/data/headscale:/data/headscale" -v "$W/data/node-control:/data/node-control" "$LS" replicate -config /etc/litestream.yml >/dev/null
sleep 15

cd "$W"
# The scripts call `docker run ... aws-cli` and `litestream` without keys: give them the test keys and endpoint.
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=testtest1
mkdir -p "$W/bin"
cat > "$W/bin/docker" <<EOF
#!/bin/sh
# Test shim: add the test server credentials and endpoint to the aws-cli and litestream containers; no compose project here.
real=\$(command -v -p docker 2>/dev/null || echo /usr/local/bin/docker)
for p in \$(echo "\$PATH" | tr ':' ' '); do [ "\$p" = "$W/bin" ] && continue; [ -x "\$p/docker" ] && real="\$p/docker" && break; done
if [ "\$1" = compose ]; then
  case "\$*" in *"config --services"*) echo headscale; exit 0;; esac
  exit 0
fi
if [ "\$1" = run ]; then
  shift
  case "\$*" in
    *"$CLI"*) exec "\$real" run -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=testtest1 "\$@" ;;
    *"$LS"*)  exec "\$real" run -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=testtest1 "\$@" ;;
  esac
  exec "\$real" run "\$@"
fi
exec "\$real" "\$@"
EOF
chmod +x "$W/bin/docker"
PATH="$W/bin:$PATH"
# aws-cli needs the endpoint: pass it through the standard env var (aws-cli >= 2.13 reads AWS_ENDPOINT_URL).
sed -i.bak "s|-e AWS_REGION=\"\$BACKUP_REGION\"|-e AWS_REGION=\"\$BACKUP_REGION\" -e AWS_ENDPOINT_URL=$EP|" backup/backup.sh backup/restore.sh

out=$(sh backup/backup.sh); echo "$out"
echo "$out" | grep -q "keys [0-9]*B" || { echo "TEST FAIL: backup.sh"; exit 1; }

R=$(mktemp -d)
RESTORE_DIR=$R sh backup/restore.sh
[ -s "$R/headscale/db.sqlite" ] || { echo "TEST FAIL: no restored db"; exit 1; }
RESTORE_DIR=$R sh backup/restore.sh | grep -q "nothing to do" || { echo "TEST FAIL: restore wrote over a database"; exit 1; }
rm -rf "$R"

d=$(sh backup/drill.sh); echo "$d"
echo "$d" | grep -q "databases and keys MATCH" || { echo "TEST FAIL: drill"; exit 1; }
echo "TEST OK"

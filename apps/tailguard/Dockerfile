# Build context: this directory (apps/tailguard). The upstream tailguard image at a pinned digest, with tailscale
# replaced by the repo pin TAILSCALE_VERSION (the official static tarball, as apps/exit-node) and the sidecar scripts
# of this directory baked in (#391). docker-compose.yml runs all three services from this image.
#   docker build --build-arg TSVERSION=$(cat ../../TAILSCALE_VERSION) -t tailguard .
ARG TSVERSION=1.104.7

FROM alpine:3.24 AS tailscale
ARG TSVERSION
ARG TARGETARCH
WORKDIR /app
RUN wget -q "https://pkgs.tailscale.com/stable/tailscale_${TSVERSION}_${TARGETARCH}.tgz" \
  && tar xzf "tailscale_${TSVERSION}_${TARGETARCH}.tgz" --strip-components=1

# Upstream: https://github.com/juhovh/tailguard (digest of its latest image on 2026-11-26). Bump the digest by hand, then
# run the image test (make test-tailguard).
FROM ghcr.io/juhovh/tailguard@sha256:51f5406131eef1fd213a0fb5e5e5aeb1232ae2719920f261217459be27eae02b
ARG TSVERSION
COPY --from=tailscale /app/tailscaled /app/tailscale /tmp/ts/
# Replace the binaries where the upstream image has them, so its entrypoint runs the pinned version. rm first: one of
# them can be a link to the other. The build fails if a binary is missing or the version is not the pin.
RUN set -e; \
  for b in tailscale tailscaled; do \
    p=$(command -v "$b") || { echo "upstream image has no $b on PATH" >&2; exit 1; }; \
    rm -f "$p"; cp "/tmp/ts/$b" "$p"; chmod 0755 "$p"; \
  done; \
  rm -rf /tmp/ts; \
  tailscale version | head -n1 | grep -qx "$TSVERSION"; \
  tailscaled --version | head -n1 | grep -qx "$TSVERSION"
COPY route-health.sh track-exit-node.sh verify.sh /usr/local/bin/
# Unsets an empty TG_WEBUI_PORT before the upstream entrypoint, which exits on it (see entrypoint.sh).
COPY --chmod=0755 entrypoint.sh /usr/local/bin/headlamp-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/headlamp-entrypoint.sh"]

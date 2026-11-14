# Build context: this directory (apps/tailnet-connector). tailscale at the repo pin TAILSCALE_VERSION, from the official
# static tarball (as apps/exit-node), plus a small entrypoint for Kubernetes. Used by charts/tailnet-connector (#103).
#   docker build --build-arg TSVERSION=$(cat ../../TAILSCALE_VERSION) -t tailnet-connector .
ARG TSVERSION=1.104.5

FROM alpine:3.24 AS tailscale
ARG TSVERSION
ARG TARGETARCH
WORKDIR /app
RUN wget -q "https://pkgs.tailscale.com/stable/tailscale_${TSVERSION}_${TARGETARCH}.tgz" \
  && tar xzf "tailscale_${TSVERSION}_${TARGETARCH}.tgz" --strip-components=1

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=tailscale /app/tailscaled /app/tailscale /usr/local/bin/
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
# Userspace mode: no capabilities, no /dev/net/tun; runs as the pod's user (the chart sets 1000).
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

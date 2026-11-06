# The TLS edge: the official Caddy image, plus the DNS plugins named in DNS_PLUGINS.
#
# Caddy gets its certificates from Let's Encrypt. Fixed public names (headscale, enrolment) use HTTP-01 on port 80 and
# need no plugin, on any host and any cloud. Only the funnel wildcards (*.<namespace>.<domain>) need DNS-01, and so a
# plugin for the DNS provider of that domain. See the README, "The edge and its certificates".
#
# DNS_PLUGINS is a space-separated list of Go modules. Empty (the default) builds the plain official Caddy binary.
#   github.com/caddy-dns/route53@v1.6.0      Route 53 (AWS)
#   github.com/caddy-dns/cloudflare@v0.2.1   Cloudflare
#   github.com/caddy-dns/googleclouddns@v1.1.0   Google Cloud DNS
#   github.com/caddy-dns/rfc2136@v1.0.0      any DNS server that accepts RFC 2136 updates (BIND, PowerDNS, Knot)
# Pin the versions; rebuild to upgrade.
ARG CADDY_VERSION=2.10
FROM caddy:${CADDY_VERSION}-builder AS build
ARG DNS_PLUGINS=""
RUN set -e; args=""; for m in $DNS_PLUGINS; do args="$args --with $m"; done; \
    xcaddy build --output /out/caddy $args

FROM caddy:${CADDY_VERSION}
COPY --from=build /out/caddy /usr/bin/caddy

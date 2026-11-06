# Caddy with the Route 53 DNS provider, for the wildcard certificates of funnel-ingress (ACME DNS-01). It reads AWS
# credentials from the instance role. Pin both versions; rebuild to upgrade.
FROM caddy:2.10-builder AS build
RUN xcaddy build --with github.com/caddy-dns/route53@v1.6.0

FROM caddy:2.10
COPY --from=build /usr/bin/caddy /usr/bin/caddy

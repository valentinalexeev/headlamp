# The TLS edge: the official Caddy image plus one plugin, caddy-dns/route53. Caddy gets its certificates from
# Let's Encrypt. Fixed public names (headscale, enrolment) use HTTP-01 and need no plugin. Wildcard public names
# (funnel, *.<namespace>.<domain>) need DNS-01: Caddy writes a TXT record in Route 53, through this plugin, with the
# credentials of the instance role. Only funnel needs it. Pin both versions; rebuild to upgrade.
# For another DNS provider, replace the --with module and `dns route53` in the Caddyfile.
FROM caddy:2.10-builder AS build
RUN xcaddy build --with github.com/caddy-dns/route53@v1.6.0

FROM caddy:2.10
COPY --from=build /usr/bin/caddy /usr/bin/caddy

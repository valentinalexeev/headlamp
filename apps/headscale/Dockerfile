# The headscale image: the stock upstream image, unchanged, with the placeholder site config for the Fly overlay
# (the overlay copies the site's headscale.yaml and acl.json over these). The upstream image is distroless: it has
# no shell, so run commands with `flyctl ssh console -C "headscale ..."` or `docker exec <ctr> headscale ...`.
# Routes are approved by the policy (autoApprovers in acl.json), so the image needs no API key and no scripts.
# HEADSCALE_VERSION must equal the HEADSCALE_VERSION file (scripts/check-headscale-pin.sh).
ARG HEADSCALE_VERSION=v0.29.7
FROM ghcr.io/juanfont/headscale:${HEADSCALE_VERSION}

COPY ./headscale.yaml /etc/headscale/config.yaml
COPY ./acl.json /etc/headscale/acl.json

ENTRYPOINT ["/ko-app/headscale"]
CMD ["serve"]

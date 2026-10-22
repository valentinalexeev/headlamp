# Pin the headscale image: the build needs a shell and apk (route approver scripts, `flyctl ssh console -C`).
# jauderho/headscale:latest is NOT usable from v0.29.5 on (that image has no /bin/sh), so the default is the
# last Alpine-based tag we ran. Override with: fly/deploy.sh headscale --build-arg HEADSCALE_VERSION=<tag>
ARG HEADSCALE_VERSION=v0.29.3
FROM jauderho/headscale:${HEADSCALE_VERSION}

RUN apk add --no-cache curl bash jq ca-certificates

COPY approve-gateway-routes.sh /usr/local/bin/approve-gateway-routes.sh
COPY cron-approve-routes.sh /usr/local/bin/cron-approve-routes.sh

RUN chmod +x /usr/local/bin/approve-gateway-routes.sh \
    /usr/local/bin/cron-approve-routes.sh

COPY ./headscale.yaml /etc/headscale/config.yaml
COPY ./acl.json /etc/headscale/acl.json

ENTRYPOINT []
